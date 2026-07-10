# Disaster recovery (TAV-31)

How to back up and restore every piece of durable ProteOS state, and how to
recover from the two failure modes that matter: **the app VM's disk dies** and
**a KVM host's disk dies**. Before this runbook, a single disk failure was
unrecoverable data loss — there was no backup story at all.

For the normal deploy path see [RUNBOOK.md](../RUNBOOK.md); this doc only
covers backup/restore.

## What's backed up, and by what

| Component                        | Holds                                                              | Runs on     | Backup script                                    | Restore script                                    |
| --------------------------------- | ------------------------------------------------------------------- | ----------- | ------------------------------------------------- | -------------------------------------------------- |
| **Postgres**                      | users, machines, hosts, disks, events, audit log — the source of truth | app VM      | `deploy/app-stack/backup-postgres.sh`             | `deploy/app-stack/restore-postgres.sh`             |
| **OpenBao** (`baodata` volume)    | per-machine LUKS volume keys, per-user provider API keys/GitHub tokens, the AppRole/audit config | app VM | `deploy/app-stack/backup-openbao.sh`              | `deploy/app-stack/restore-openbao.sh`              |
| **LUKS volumes** (`*.luks`)       | each machine's writable rootfs, persistent disk (`/dev/vdb` in the guest), and any hibernate snapshot | KVM host(s) | `deploy/node-agent/backup-volumes.sh`             | `deploy/node-agent/restore-volumes.sh`             |

A convenience wrapper `deploy/app-stack/backup-all.sh` runs the Postgres and
OpenBao backups back to back (both live on the app VM). The volume backup is
separate because it runs on a different machine (the KVM host).

**Not backed up, and why that's fine:**

- The node-agent's own state dir (`PROTEOS_AGENT_DATA_DIR`, `state.json` +
  the IP allocator) — Postgres is authoritative for machine state; the
  node-agent's copy is a local cache the poller repopulates.
- The jailer chroot tree (`PROTEOS_CHROOT_BASE_DIR`) — rebuilt fresh per boot,
  holds no durable data (the durable bits live in the `.luks` container,
  bind-mounted in as `/state`).
- Machine images (`PROTEOS_AGENT_IMAGES_DIR`: kernel/rootfs) — these are build
  artifacts of `image/build-rootfs.sh` / the Ansible bake, reproducible from
  the pinned `image/manifest.lock`, not operator data.

**Conditionally not backed up — check this applies to you:** the `cpdata`
volume (`/var/lib/proteos` in the `controlplane` container) holds
`secrets.json`, the **`file`-backend** secrets store (`PROTEOS_SECRETS_BACKEND`
defaults to `file`; RUNBOOK Part D). Everything above assumes production has
been switched to `PROTEOS_SECRETS_BACKEND=openbao` per Part D, in which case
`cpdata` holds no secrets and skipping it is fine. **If your stack is still on
the `file` backend, you have no backup story for its secrets** — either
complete the OpenBao migration (Part D2/D3), or back up `cpdata` the same way
`backup-openbao.sh` backs up `baodata`: `docker run --rm -v
proteos_cpdata:/d:ro -v "$PWD":/b alpine tar czf /b/cpdata.tgz -C /d .`.

## Why three separate backups, and why they must travel together

A machine's persistent disk is useless without three things agreeing:

1. The **Postgres** `machines`/`disks` row (which host, which disk size, which
   image refs).
2. The **OpenBao** volume key at `secret/machines/<machine_id>/volume-key`
   (without it, `cryptsetup luksOpen` on the restored container fails).
3. The **LUKS container file** itself (`<machine_id>.luks`) on the right KVM
   host.

Restoring only one or two of these leaves you with either ciphertext you can't
open, or a database row pointing at a disk that no longer exists. **Always
restore Postgres and OpenBao together, from backups taken close in time to
each other**, before touching volume restores. See the two disaster scenarios
below for the exact order.

## Where backups live, and why that alone is not a backup

Every script defaults to a local directory under `/var/backups/proteos/...`,
separate from the data volumes/dirs, so a `docker compose down -v` or a
`rm -rf` of the wrong directory doesn't take out the backups with it. **That
is not enough on its own**: a local backup directory is still on the same
physical disk as the primary data, and this whole effort exists because "a
single disk failure is currently unrecoverable data loss." Set the
`*_BACKUP_REMOTE` variable in each `backup.env` (see the `.env.example` files
next to each script) to an `rsync`-reachable off-host destination — a second
host, a NAS, object storage mounted via `rclone`/`s3fs`, whatever you have.
Every backup script pushes there as its last step if the variable is set.
**Until you set it, you have faster recovery from operator error, not disaster
recovery.**

## Running backups

### One-shot, manually

```bash
# App VM:
cd deploy/app-stack
./backup-all.sh                 # postgres + openbao (openbao briefly restarts)
# or individually:
./backup-postgres.sh
./backup-openbao.sh             # add --online to skip the brief openbao restart

# KVM host (each one that has machines):
cd deploy/node-agent
sudo ./backup-volumes.sh
```

### On a schedule (systemd timers)

Each host gets a `oneshot` service + a daily `timer`. Install steps are in the
header comment of each `.service` file:

- App VM: `deploy/app-stack/proteos-backup.service` + `.timer` (03:00 UTC,
  ±15 min jitter) — runs `backup-all.sh`.
- Each KVM host: `deploy/node-agent/proteos-backup-volumes.service` + `.timer`
  (03:30 UTC, offset from the app-VM timer so they don't contend for the same
  remote at once) — runs `backup-volumes.sh`.

```bash
sudo systemctl enable --now proteos-backup.timer            # app VM
sudo systemctl enable --now proteos-backup-volumes.timer    # each KVM host

# Confirm they're scheduled and check the last run:
systemctl list-timers 'proteos-backup*'
journalctl -u proteos-backup.service -u proteos-backup-volumes.service --since -2d
```

Both `.service` units read `/etc/proteos/backup.env` (optional —
`EnvironmentFile=-...`, the leading `-` makes a missing file a no-op) for
overrides: retention, the off-host `*_BACKUP_REMOTE` target, or a
non-default `PROTEOS_BAODATA_VOLUME`. Copy the matching `backup.env.example`
there to start.

### Consistency notes (read before you rely on this)

- **Postgres**: `pg_dump --format=custom` against a live database is
  transactionally consistent (a single snapshot of the whole dump), no
  downtime needed.
- **OpenBao**: the `file` storage backend has no snapshot primitive (that's
  raft-only). The default backup briefly stops `openbao` + `bao-unsealer`
  (seconds to low minutes depending on `baodata` size) for a clean tar of the
  volume — schedule it for a quiet window, or pass `--online` to skip the stop
  if you accept a best-effort-consistent copy (each KV entry is its own file
  written via rename, so in practice this is usually fine, but it has not been
  proven point-in-time consistent the way the default path is).
- **LUKS volumes**: copied live via `rsync`, which is **crash-consistent, not
  application-consistent** for a volume whose mapper is currently open (the
  machine is running) — the same guarantee as if you'd pulled power on that
  VM's disk mid-write; ext4's journal replay handles it on next open. For a
  fully quiesced copy of a specific machine, stop or hibernate it first, then
  run the backup. `backup-volumes.sh` logs which volumes were hot vs. cold at
  backup time.

## Restore scenario 1: app VM disk dies (Postgres + OpenBao lost)

The KVM host(s) and their `.luks` volumes are intact; only the control plane's
state is gone.

1. Provision a fresh app VM per RUNBOOK Part B (Docker + Compose, `.env`
   filled in with the **same** `PROTEOS_AGENT_TOKEN`, `PROTEOS_NODE_AGENT_URL`,
   etc. as before — these are operator config, not backed-up data).
2. Bring up just the data services:
   ```bash
   cd deploy/app-stack
   touch openbao-secret-id bao-unseal-key   # placeholders for the bind-mounts
   docker compose up -d postgres openbao bao-unsealer
   ```
3. Restore OpenBao **first** (the control plane's secrets self-check on boot
   will fail without it):
   ```bash
   ./restore-openbao.sh --latest
   ```
4. Restore Postgres:
   ```bash
   ./restore-postgres.sh --latest
   ```
5. Bring up the rest of the stack and verify:
   ```bash
   docker compose up -d
   docker compose logs controlplane | grep -E 'secrets backend|secrets self-check|applying migrations'
   docker compose exec postgres psql -U proteos -d proteos -c 'select count(*) from machines;'
   BAO_TOKEN=$(jq -r .root_token openbao-init.json) \
     bao kv get secret/proteos/machines/<a-machine-id>/volume-key   # spot-check a key survived
   ```
6. Confirm the node-agent is still reachable (`curl -sk
   https://<node-tailnet-ip>:9090/healthz`) and that existing machines still
   report `running`/`stopped` correctly on the dashboard — the poller
   reconciles against the node-agent's live state on the next tick.

**RPO** here is the gap between the two backups' timestamps (worst case: just
under 24h with the default daily timer, since Postgres and OpenBao are backed
up back-to-back by `backup-all.sh` they're always close together). **RTO** is
however long it takes to provision a VM + run the four commands above —
budget 15–30 minutes.

## Restore scenario 2: a KVM host's disk dies (LUKS volumes lost)

Postgres and OpenBao are intact; that host's machines lost their persistent
disks. The `machines`/`disks` rows still exist and still point at this host,
but `<machine_id>.luks` is gone.

1. Provision/repair the KVM host per RUNBOOK Part A (Firecracker spike,
   node-agent install) and stop the node-agent before restoring
   (`systemctl stop proteos-node-agent`) so nothing tries to open a volume
   mid-copy.
2. Restore the volumes:
   ```bash
   cd deploy/node-agent
   sudo ./restore-volumes.sh --latest --all
   # or a single machine:
   sudo ./restore-volumes.sh --latest --machine <machine-id>
   ```
3. Start the node-agent:
   ```bash
   sudo systemctl start proteos-node-agent
   ```
4. From the dashboard (or CLI), stop then start each affected machine. This
   forces a fresh `ensure` call, which delivers the volume key from OpenBao
   and `luksOpen`s the restored container. Confirm on the guest that the
   persistent disk (`/dev/vdb`, mounted workspace) has last-backup contents,
   not empty — **any writes since the last volume backup are lost** (this is
   the actual RPO of this path, not the Postgres/OpenBao one).

If the volume backup and the current OpenBao secrets have drifted (e.g. a
machine's volume key was rotated after the last volume backup — not something
ProteOS does today, but worth knowing), `luksOpen` fails with a decryption
error; there is no recovery from that combination other than treating the
machine as lost and creating a new one.

## Restore scenario 3: everything is gone

Restore in this order — Postgres/OpenBao first (they're the source of truth
for what machines *should* exist and the keys to open them), then volumes:

1. Scenario 1, steps 1–5 (fresh app VM, Postgres + OpenBao restored).
2. Scenario 2, steps 1–3, once per KVM host.
3. Stop/start every machine from the dashboard to re-open its restored volume.

## Verifying a backup is actually restorable

A backup you've never restored is a hope, not a backup. Periodically (e.g.
quarterly, or after any change to the backup scripts):

- Spin up a scratch app VM, run scenario 1 against it pointed at a copy of the
  latest backups, and confirm the dashboard shows the expected machines.
- Run `restore-volumes.sh` against a scratch directory (`PROTEOS_AGENT_VOLUMES_DIR`
  pointed elsewhere) and confirm `cryptsetup luksOpen` succeeds with the key
  fetched from the restored OpenBao.

## Gotchas

- **`docker compose down -v` deletes the `pgdata`/`baodata`/`cpdata` volumes.**
  Never run this on a host you intend to keep; if you do, `docker compose up -d`
  brings services back with empty volumes and you're immediately in restore
  scenario 1.
- **The `openbao-init.json`, `bao-unseal-key`, and `openbao-secret-id` files
  are gitignored on purpose** (RUNBOOK Part D1) — they hold the unseal key and
  root token. `backup-openbao.sh` bundles them alongside the volume tarball
  precisely because they're nowhere else; losing them without a backup means
  the restored `baodata` volume can never be unsealed again (OpenBao's
  single-key-share Shamir setup has no other recovery path).
- **Retention prunes local copies only up to `*_RETENTION_DAYS`/`*_RETENTION`**;
  if `*_BACKUP_REMOTE` uses `--delete` (postgres/openbao do; the volume backup
  intentionally does not, since remote pruning of hardlinked snapshots needs
  the same `--link-dest` machinery run remotely), the remote mirrors local
  retention too. Set retention high enough to survive the time it'd take you
  to notice backups have stopped running.
- **A LUKS volume backup taken while a machine is running is crash-consistent,
  not a true point-in-time snapshot** — see "Consistency notes" above. This is
  an accepted tradeoff for a portable, dependency-free backup path; if the
  KVM host's filesystem supports LVM/btrfs/ZFS snapshots, taking a filesystem
  snapshot immediately before the rsync pass (then backing up the frozen
  snapshot) upgrades this to application-consistent without any script
  changes — snapshot, run `backup-volumes.sh` against the snapshot mount, drop
  the snapshot.
