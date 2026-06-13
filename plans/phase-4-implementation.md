# Phase 4 Implementation Plan: Persistent disk + hibernate/resume

> Source: `plans/proteos-poc-to-prod.md` Phase 4, planned 2026-06-11.
> Status: **Track A (Mac/dev-driver) landed** — 4.1 node-agent contract + DevDriver,
> 4.2 control-plane schema/lifecycle/key-broker, 4.3 guest-agent persist+SQLite+resume,
> 4.5 dev-stack e2e + React. `go test ./...` green across all three modules (incl. the
> `TestHibernateResumeE2E` dev-stack e2e: file survives stop/start, `boot:resumed`).
> **4.4 FirecrackerDriver landed + verified on Proxmox** —
> `volume.go` (LUKS2 provision/open/mount/close, sizing), `snapshot.go` (pause+create,
> load, version guard, consume, guest `/resume` hook), the `prepareChroot` →
> `prepareColdJail`/`prepareResumeJail` split (rootfs now lives on the encrypted
> `/state` volume, not the jail), cold-vs-resume dispatch with FC-version-mismatch /
> restore-error cold-boot fallback, `Destroy`/`Reattach`/`cleanupHost` volume handling.
> Cross-builds + `go vet` clean under `-tags firecracker`. The gated
> `TestHibernateResumeCycle` exercises the full volume/snapshot lifecycle on KVM and now
> also enforces the two acceptance proofs in CI: **encrypted at rest** (a plaintext probe
> written to the open volume is absent from the raw closed `.luks`; `cryptsetup isLuks`)
> and **resume hygiene** (the guest `/resume` outcome is surfaced as
> `Status.ResumeHygiene`/`ResumeSkewMS`; asserted `== "ok"` when the runner sets
> `PROTEOS_TEST_ROOTFS_HAS_GUEST_AGENT=1` against the baked rootfs).
> `run-node-agent.sh` + `.env.example` updated (volumes dir, cryptsetup, optional TLS).
> **Host provisioning via Ansible (`deploy/ansible/`)** — installs cryptsetup, creates the
> `volumes` dir `0700` outside the jail tree, renders the env (volumes dir, cryptsetup,
> optional TLS), bakes the 4.3 guest agent into the rootfs and re-pins `PROTEOS_ROOTFS_REF`.
> **Rootfs tooling updated for Phase 4** — `image/build-rootfs.sh` bakes the 4.3
> guest agent (persist + SQLite + `/resume`, built from source), the systemd unit
> documents disk mode (`/dev/vdb` → `/persist`, runs as root), the build warns if the
> base lacks `fsck.ext4`, and the release stamp / manifest record
> `features=terminal,persist,resume`. Guest `fsck` is best-effort so a missing binary
> degrades to a journal-replay mount rather than disabling persistence. Run the script
> on the Proxmox/Linux host and re-pin `PROTEOS_ROOTFS_REF` to the new `ga<gitshort>`.
> **4.0 spike landed** — `09-encrypted-disk.sh` (encrypted hibernate/resume cycle) +
> `10-measure-findings.sh` (boot/snapshot/restore/cgroup timings → committable
> `findings.{json,md}`); the README findings table is populated (CRNG reseeds via VMGenID;
> ~16 s skew ≈ hibernated dwell).
> **Track B complete — `TestHibernateResumeCycle` green on the Proxmox KVM box**
> (`PROTEOS_TEST_ROOTFS_HAS_GUEST_AGENT=1` against the baked rootfs): full cold-boot →
> hibernate → resume, with the two acceptance proofs enforced — encrypted-at-rest (probe
> absent from the raw closed `.luks`; `isLuks`) and resume hygiene (`ResumeHygiene="ok"`,
> guest corrected ~6 s skew + reseeded entropy). The KVM shakeout fixed two real bugs:
> the test's chroot base overflowed the AF_UNIX 108-byte socket path (short base dir), and
> `finishStop` raced the VMM's death so `luksClose` hit "device still in use" (wait for
> SIGKILL'd VMM to exit before umount + retry `luksClose`). **Both master-plan Phase-4
> acceptance boxes are now ticked.** Optional follow-up: commit `encrypted-findings.*` from
> a `09` run for a stored artifact.
> Prerequisites: Phase 2 (driver interface, jail layout, state store, lifecycle poller) and
> Phase 3 (guest agent + vsock tunnel + gateway) — both landed. Phase 4 treats their
> contracts as given and extends them; it does not rework the boot, tunnel, or gateway paths.
>
> Spike findings that gate this phase (from `spike/firecracker/README.md`, runs of
> `04-attach-disk.sh`, `05-snapshot-restore.sh`, `08-vsock.sh`):
> disk persistence across cold boot is proven; snapshot restore requires the **same
> Firecracker version** and the **same tap name**; the stale vsock uds must be **removed
> before `LoadSnapshot`** ("Address in use" otherwise) and Firecracker re-creates it;
> in-flight connections never survive restore (gateway re-dials — already its model);
> clock skew ≈ hibernated duration and CRNG reseed behavior are **observed but not yet
> recorded in the findings table** — Task 4.0 closes that.

## Context

Phases 2–3 boot a jailed Firecracker VM with a **fresh rootfs copy per boot**
(`prepareChroot` deletes the whole jail subtree — `nodeagent/internal/driver/firecracker/jail.go:46`)
and give it an interactive terminal. Nothing survives a stop. Phase 4 makes the machine
*durable*: a per-machine persistent disk (home + workspace + machine SQLite), and
**stop = pause + snapshot (hibernate)**, **start = restore (resume)** with cold boot as the
fallback. The demo: run `top` in the terminal, write a file, Stop, Start — the file is
there *and `top` is still running*, in the same shell, with scrollback.

Two facts force the central design move:

1. **A snapshot's guest RAM references the rootfs backing file.** Restore needs the
   *identical* rootfs bytes that existed at pause time, so the per-boot-fresh rootfs copy
   must be preserved across hibernate. The rootfs is therefore mutable per-machine state,
   not a disposable scratch copy.
2. **Snapshots contain guest RAM and the rootfs accumulates user data** — the master
   plan's security baseline requires both encrypted at rest and handled as secret
   material. The guest must also never be able to write the snapshot files (a tampered
   snapshot is parsed by the VMM at restore — attack surface), so they cannot live on the
   guest-writable disk.

Both are solved by one construct, decision #1 below.

## Architecture decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | **One LUKS2 "machine volume" per machine** — a container file at `PROTEOS_AGENT_VOLUMES_DIR/<machine-id>.luks`, opened to `/dev/mapper/proteos-<id8>`, ext4, mounted by the node-agent at `<jail-root>/state` (plain mount under the jail path; same mount ns, so the chrooted VMM sees `/state`). It holds **all mutable per-machine state**: `data.ext4` (the persistent disk presented to the guest as `/dev/vdb`), `rootfs.ext4` (the writable rootfs copy, preserved across hibernate, recreated from the pinned image only on cold boot), and `snap/{vmstate,mem}` (snapshot files, written by Firecracker directly onto the encrypted mount — never plaintext at rest). Sized `rootfs-image + mem_mib + disk_mib + 512 MiB` slack. | One LUKS layer covers disk **and** snapshot **and** rootfs encryption in a single mechanism; Firecracker only ever opens regular files (`path_on_host=/state/...`), so no block-device `mknod` into the chroot and no second container. Snapshot files sit outside the guest-writable `data.ext4` (tamper-proof). `prepareChroot`'s delete-everything becomes "delete jail scaffolding, never the volume" — the volume file lives outside the jail tree. Disk provisioning stays behind the driver/`Disk` abstraction (master-plan open decision: network block storage can later replace the file path without touching the control plane). |
| 2 | **Volume key minted and held by the control plane**, not the host: 32 random bytes per machine, created at machine-create, stored in the Phase 1 `secrets.Store` at `machines/<machine_id>/volume-key` (FileStore now; the path matches the OpenBao convention so Phase 5 swaps the backend, not the callers). Delivered to the node-agent **in each `EnsureRequest`** (and only there), held in memory for `luksOpen`, never persisted host-side, never logged. | Key-next-to-ciphertext (host-local keyfile) defeats at-rest encryption against the realistic threats (leaked host backups, removed disks). The control plane already brokers secrets per the master plan (`secret/machines/<machine_id>/…`). Ensure is the only call that needs it: stop/hibernate operates on already-open volumes. |
| 3 | **The node-agent channel gets TLS** because it now carries key material: node-agent serves HTTPS when `PROTEOS_AGENT_TLS_CERT/KEY` are set; the control plane verifies against a pinned CA/cert via `PROTEOS_NODE_CA_FILE`. Bearer token unchanged on top. Dev stack (loopback/Mac) stays plain HTTP. | A bearer-authenticated **plaintext** LAN channel was fine for lifecycle verbs; it is not fine for volume keys. Self-signed pinned cert = no PKI machinery; ~30 lines on each side. |
| 4 | **Lifecycle: stop = hibernate.** `POST /api/machine/stop` → `running → hibernating` (new transitional state — already in the DB CHECK constraint and the web `MachineState` union) → agent pauses the VM, `PUT /snapshot/create` (Full) to `/state/snap/`, kills the VMM, unmounts + `luksClose` → `stopped` with a snapshot recorded. `POST /api/machine/start` → `starting` → agent `luksOpen` + mount, recreates the tap **with the persisted same name**, launches a fresh jailed VMM, **`rm` the stale `v.sock`**, `PUT /snapshot/load` (`mem_backend: File`, `resume_vm: true`) → `running`, `boot=resumed`. Snapshot files are **deleted after a successful resume** (stale RAM must never be restored twice) and the Postgres snapshot row removed. `stopping` remains the cold/poweroff path (CtrlAltDel), used as **automatic fallback** when snapshot create fails; restore failure (FC version mismatch, corrupt snapshot) falls back to a cold boot with `rootfs.ext4` recreated and `data.ext4` untouched — sessions lost, files kept, `machine_events` payload says why. | Matches the master-plan state machine (`running → hibernating → stopped`) and the spike's hard findings (same-FC-version, same-tap-name, rm-stale-uds). Hibernate-by-default is the product behavior; cold stop "for cost" is deferred until something needs it. Fallbacks mean Stop/Start never wedge a machine because snapshotting misbehaved. |
| 5 | **Resume-or-boot is the driver's decision, not the control plane's.** `EnsureRunning` stays "make it running": the FC driver checks its persisted `Record` for a snapshot (id, FC version) and resumes if compatible, else cold-boots. The control plane learns what happened from `Status`, which gains `boot: "cold"|"resumed"` and `snapshot: {present, created_at, fc_version, mem_bytes}`; the poller records both (event payload + `snapshots` table). | The node-agent is the only component that knows FC versions, file presence, and jail state. Keeping the control plane on the existing ensure/poll contract means zero new orchestration verbs and the poller pattern (`machine/poller.go:104`) extends, not changes. |
| 6 | **Postgres:** migration `000003`: `disks` table (`id uuid PK, machine_id uuid UNIQUE FK, size_mib, created_at`), `machines.disk_id uuid FK` (set at create), `snapshots` table (`machine_id uuid PK FK, fc_version text, mem_bytes bigint, kernel_ref text, rootfs_ref text, created_at`) holding **the current snapshot only** (row deleted on consume/cold-stop; history lives in `machine_events`). `resource_spec` jsonb gains `disk_mib` (default 10240). | Satisfies "disk_id and snapshot metadata are recorded in Postgres" with the master plan's data-model shape. One-current-row keeps the poller upsert trivial; events already audit every transition. A `disks` table (vs. a column) is the seam Phase 12 backups and future network storage need. |
| 7 | **Guest persistence layout:** the guest agent (already root, already first-up via systemd) gains a startup `persist` step: wait ≤10 s for `/dev/vdb`, `fsck -p`, mount at `/persist`, ensure `home/`, `workspace/`, bind-mount `/persist/home → /root` and `/persist/workspace → /workspace`, then start accepting sessions (shell `$HOME` and workspace are thereby on the disk). Missing device → log loudly and run degraded (ephemeral) rather than refuse terminals. Dev override: `PROTEOS_GUEST_PERSIST=/path` uses a plain directory and skips mounting. | Mounting in the guest agent (not fstab in the image) keeps the pinned rootfs image unchanged and the logic testable; first-boot mkfs is host-side (node-agent, like spike 04) so the guest never formats. On resume none of this re-runs — mounts live in the restored RAM, and the reopened `data.ext4` bytes are identical (volume closed only after VMM death). |
| 8 | **Machine SQLite** at `/persist/machine.db` via `modernc.org/sqlite` (pure Go — guest agent stays `CGO_ENABLED=0` static). Phase 4 schema: `schema_version`, `boots` (boot_id, kind cold/resumed, ts), and a `kv` table; the guest agent records every boot/resume and exposes the latest over a small `GET /info` (vsock) for tests. Phase 9 (window layout, session index) extends this schema rather than inventing storage. | "Initialized on the disk and used by the guest agent" with an honest, minimal use; pure-Go driver avoids cgo cross-build pain in `image/build-rootfs.sh`. |
| 9 | **Resume hygiene is explicit, not assumed:** after `LoadSnapshot`, the node-agent calls a new guest-agent endpoint `PUT /resume` (HTTP over the existing vsock listener; vsock trust per Phase 3 decision #10) with `{unix_nanos, entropy_b64 (32 B)}`. The guest agent does `clock_settime(CLOCK_REALTIME)` and `ioctl(RNDADDENTROPY)`, records a `resumed` boot row, and returns the skew it corrected. Spike 09 checks whether Firecracker's VMGenID on the pinned kernel (6.1.155) already triggers a CRNG reseed on restore (`dmesg | grep -i 'crng\|vmgenid'`); if yes, the entropy injection stays as belt-and-braces, if no, it's load-bearing. Clock resync is load-bearing either way (spike 05: skew ≈ hibernated duration; nothing resets the wall clock). | Master-plan resume criteria verbatim (TLS, git, token expiry break otherwise). Driving it from the node-agent with a host-provided timestamp is deterministic — no dependency on guest NTP egress at the resume instant. |
| 10 | **DevDriver fakes the same contract on a Mac:** per-machine persist dir `<datadir>/machines/<id>/persist` handed to the real guest agent via `PROTEOS_GUEST_PERSIST`; "hibernate" kills the guest-agent child but keeps the dir and writes fake snapshot metadata (`fc_version:"dev"`); "start" relaunches against the same dir and reports `boot:"resumed"`. No encryption in dev (real path is exercised by the FC driver + spike + KVM CI). | The entire control-plane surface (hibernating state, poller mapping, snapshot rows, SSE, React) and the guest persist/SQLite path are testable on a Mac; only the literal RAM-restore (live processes surviving) is Proxmox-only. PTY sessions do **not** survive dev hibernate — document it; on real FC they do, which is exactly what 4.6 demos. |

## Wire contracts

### agentapi (nodeagent/api) — extended

```
PUT /v1/machines/{id}                       EnsureRequest gains:
  {"vcpus":2,"mem_mib":2048,"kernel_ref":"...","rootfs_ref":"...",
   "disk_id":"<uuid>","disk_mib":10240,"volume_key_b64":"<32B b64>"}   # key never logged

POST /v1/machines/{id}/stop                 StopRequest (new body, optional):
  {"mode":"hibernate"|"poweroff"}           # default hibernate

GET /v1/machines/{id}                       MachineStatus gains:
  {"state":"...","reason":"...","handle":"...","guest_ip":"...",
   "boot":"cold"|"resumed",                 # how the current/last run started
   "disk_id":"<uuid>",
   "snapshot":{"present":true,"created_at":"...","fc_version":"v1.16.0","mem_bytes":N}}
```

### Driver interface (nodeagent/internal/driver)

```go
type Driver interface {
    EnsureRunning(ctx, spec VMSpec) (handle string, err error)   // spec.Disks now used; spec.VolumeKey []byte added
    Stop(ctx, machineID string, mode StopMode) error             // StopModeHibernate | StopModePoweroff
    Status / Destroy / List / Reattach                           // Status gains Boot, Snapshot, DiskID
}
// Disk{ID string, SizeMiB int} — single entry this phase; Destroy deletes the volume + record.
```

### Firecracker sequences (FC driver, per spike 04/05/08 + new 09)

```
provision (first ensure):  truncate <volumes>/<id>.luks → luksFormat (key via stdin)
                           → luksOpen → mkfs.ext4 mapper → mount <root>/state
                           → truncate state/data.ext4 (disk_mib) → mkfs.ext4 data.ext4
cold boot:                 cp pinned rootfs → state/rootfs.ext4; PUT /drives/rootfs
                           {path:/state/rootfs.ext4}; PUT /drives/data
                           {path:/state/data.ext4, is_root_device:false}; … InstanceStart
hibernate (stop):          PATCH /vm {"state":"Paused"} → PUT /snapshot/create
                           {snapshot_type:"Full", snapshot_path:"/state/snap/vmstate",
                            mem_file_path:"/state/snap/mem"} → kill VMM → umount → luksClose
resume (start):            luksOpen + mount → tap recreated with persisted same name
                           → launch jailer → rm <root>/v.sock (spike 08: mandatory)
                           → PUT /snapshot/load {snapshot_path, mem_backend:{File}, resume_vm:true}
                           → guest PUT /resume {unix_nanos, entropy_b64}
                           → rm state/snap/* (consumed)
guards:                    snapshot.fc_version != installed FC → cold boot fallback;
                           any restore error → recreate rootfs.ext4, cold boot, event notes fallback
```

### Guest agent (guestwire) — new control surface

```
PUT /resume   {"unix_nanos":N,"entropy_b64":"..."}  → 200 {"skew_corrected_ms":N}
GET /info     → {"version":"...","persist":"disk"|"dir"|"none","last_boot":{"kind":"cold|resumed","ts":...}}
```

### Config additions

```
nodeagent:    PROTEOS_AGENT_VOLUMES_DIR=/var/lib/proteos/volumes,
              PROTEOS_AGENT_TLS_CERT / PROTEOS_AGENT_TLS_KEY (optional; dev = plain HTTP),
              PROTEOS_CRYPTSETUP_BIN=/usr/sbin/cryptsetup
controlplane: PROTEOS_NODE_CA_FILE (pin agent TLS), default resource_spec gains disk_mib=10240
guestagent:   PROTEOS_GUEST_PERSIST (dev dir override), PROTEOS_GUEST_PERSIST_DEV=/dev/vdb
```

## Package layout (new / touched)

```
spike/firecracker/09-encrypted-disk.sh    # LUKS volume + bind layout + snapshot/restore + vmgenid/clock obs
nodeagent/
  api/agentapi.go                         # EnsureRequest/StopRequest/MachineStatus extensions
  internal/driver/driver.go               # StopMode, VMSpec.VolumeKey, Status fields
  internal/driver/firecracker/
    volume.go                             # NEW: luksFormat/Open/Close, mount, provision, mkfs, sizing
    snapshot.go                           # NEW: pause+create, load, consume, version guard
    firecracker.go / jail.go              # boot-vs-resume split; prepareChroot stops deleting state
  internal/driver/dev/dev.go              # persist dir, fake hibernate/resume metadata
  internal/state/state.go                 # Record: DiskID, Snapshot{FCVersion, MemBytes, CreatedAt}, Boot
  internal/httpapi/                       # stop body, status fields; TLS listener
controlplane/
  migrations/000003_disks_snapshots.*.sql
  internal/store/queries.sql              # disks/snapshots CRUD; sqlc regen
  internal/machine/{state,service,poller}.go  # hibernating transitions; key fetch at ensure; status→rows
  internal/secrets/                       # MintMachineVolumeKey helper (path machines/<id>/volume-key)
  internal/nodeclient/nodeclient.go       # stop mode, status fields, TLS pinning
guestagent/
  internal/persist/                       # NEW: device wait/fsck/mount/binds; dir mode; SQLite init (modernc)
  internal/server/                        # PUT /resume (clock_settime, RNDADDENTROPY), GET /info
web/src/
  api/client.ts                           # MachineSummary: disk/snapshot/boot fields
  components/MachineCard.tsx              # hibernating spinner state; "resumed/cold" + disk chip
image/build-rootfs.sh                     # rebuild: guestagent with persist + sqlite (image version bump)
```

## Tasks (Track A = Mac/dev-driver; Track B = Proxmox/Firecracker)

### 4.0 — Spike `09-encrypted-disk.sh` + close the 04/05 findings table (Track B; standalone, do first)
The risk-hotspot burn-down. Script: create LUKS2 container → open → ext4 → mount under the
jail root → `data.ext4` + rootfs copy on it → jailed boot with both drives as `/state/*`
files → write file in guest on `/dev/vdb` → hibernate sequence (pause, snapshot to
`/state/snap/`, kill, umount, luksClose) → reopen, fresh jailed VMM, rm stale `v.sock`,
`LoadSnapshot` → verify: file present, **pre-hibernate background process still running**,
vsock echo works. Record: whether VMGenID reseeds CRNG on the pinned kernel, observed
clock skew, snapshot create/restore timings, mem-file size, any jailer-vs-mount surprise.
Also fill the empty 04/05 rows in the spike README findings table.
**Done when:** the full encrypted hibernate/resume cycle passes scripted on the Proxmox VM
and the findings table has no empty Phase-4 rows.

### 4.1 — node-agent contract + DevDriver (Track A)
`agentapi` extensions (decision #4/#5 shapes); `Driver` gets `StopMode`, `VMSpec.VolumeKey`,
`Status.Boot/Snapshot/DiskID`; `state.Record` gains disk/snapshot/boot fields; httpapi:
stop body parsing, status marshalling, **never log `volume_key_b64`** (redact middleware
test); optional TLS listener. DevDriver per decision #10 (persist dir, kill/relaunch
guest agent, fake metadata, `boot:"resumed"`).
**Done when:** node-agent unit tests cover hibernate→stopped→resume with metadata
round-tripping through the JSON state store and the HTTP surface; a dev machine's persist
dir survives stop/start; key material absent from logs.

### 4.2 — control plane: schema + lifecycle + key broker (Track A; after 4.1)
Migration 000003 + sqlc; state machine adds `running→hibernating`, `hibernating→stopped`,
`hibernating→error`, marks it transitional (poller fast-tick); `Service.Stop` →
hibernating + `nodes.Stop(mode=hibernate)`; `Service.Create` mints the volume key into
`secrets.Store` and allocates `disk_id`; `ensureOnAgent` fetches the key and sends it;
poller maps agent status → `snapshots` upsert/delete, `boot` into the transition event
payload; `MachineSummary` exposes disk/snapshot/boot; nodeclient TLS pinning.
**Done when:** lifecycle e2e against the fake agent (pattern of
`machine/lifecycle_test.go`) walks running→hibernating→stopped→starting→running, asserts
the snapshot row appears and is consumed, `machine_events` carries `boot:"resumed"`, and
the key reaches the fake agent on every ensure but never appears in events/summaries.

### 4.3 — guest agent: persist + SQLite + resume hook (Track A; parallel with 4.2)
`internal/persist` per decision #7 (device wait/fsck/mount/binds, dir mode, degraded
mode); SQLite init per decision #8; `PUT /resume` (clock_settime + RNDADDENTROPY behind a
linux build tag; no-op shim for mac tests) and `GET /info`; wire into startup before the
session manager.
**Done when:** unit tests (dir mode) prove db init, boot rows, kv round-trip; a Linux test
(can run in plain CI — no KVM needed for the syscall path as root in a container) proves
`/resume` sets the clock and credits entropy; degraded mode still serves terminals.

### 4.4 — FirecrackerDriver: volumes + hibernate/resume (Track B; after 4.0 + 4.1, guest hook from 4.3)
`volume.go` + `snapshot.go` per the wire-contract sequences; split `prepareChroot` into
cold-boot vs resume paths (jail scaffolding refresh never touches `/state`); persisted tap
name reuse before load; rm-stale-uds; FC-version guard + cold-boot fallback (event
reason); post-resume `PUT /resume` call + snapshot consume; `Destroy` removes volume +
record; `Reattach` handles all on-disk shapes (running VM with live mount/mapper; stopped
with snapshot; stopped cold) — node-agent restart under a running VM keeps it adoptable
because mounts/mapper are kernel state, not process state.
**Done when:** cross-builds with `-tags firecracker`; on the Proxmox VM a manual
node-agent-driven cycle does provision → cold boot → hibernate → resume with a surviving
process; `cryptsetup status` clean after stop; restore-after-binary-upgrade falls back to
cold boot with the event recorded.

### 4.5 — dev-stack e2e + React (Track A; after 4.2 + 4.3)
Full dev-stack e2e test (httptest control plane + real node-agent DevDriver + real guest
agent, per the Phase 3 3.4 harness): create → terminal → write file under `$HOME` → stop →
assert `stopped` + fake snapshot metadata → start → file present, SSE stream showed
hibernating/starting, summary shows `boot:"resumed"`. React: `hibernating` in the
transitional set, disk size + last-boot chip on MachineCard, types updated.
**Done when:** the e2e is green in normal CI and the Mac dev stack demos
file-survives-stop/start end-to-end in the browser.

### 4.6 — KVM integration test + acceptance pass (Track B; after 4.4 + 4.5)
Extend the gated `//go:build firecracker` test: boot baked rootfs → terminal session →
start `sleep`-marker process + write file on `/persist` → hibernate → assert volume
closed, snapshot files exist *inside* the volume only → resume → same WS session replays,
process alive, file present, guest clock within tolerance of host, dmesg/`/info` shows
reseed; negative: raw volume file on host contains no plaintext marker (grep). Rebuild +
re-pin the rootfs (`image/build-rootfs.sh`) with the 4.3 guest agent. Walk every master-plan
Phase 4 checkbox and tick them in `plans/proteos-poc-to-prod.md`.

### Sequencing

```
4.0 (B spike) ──────────────────────────────► 4.4 (B FC driver) ──► 4.6 (B acceptance)
4.1 (A contract/dev) ──► 4.2 (A control plane) ──► 4.5 (A e2e+web) ─┘
                    └──► 4.3 (A guest agent) ──┬──► 4.5
                                               └──► 4.4 (resume hook)
Buildable immediately in parallel: 4.0, 4.1, 4.3.
```

## Acceptance-criteria mapping (master-plan Phase 4 checklist)

| Criterion | Task |
|---|---|
| Persistent disk provisioned + attached at boot; survives stop/start and host process restarts | 4.1/4.4 (provision/attach, Reattach), 4.5 + 4.6 (survival proof) |
| `stop` snapshots/hibernates; `start` resumes (or cold-boots) with disk reattached | 4.2 (lifecycle) + 4.4 (FC sequences, fallbacks) + 4.6 |
| Machine SQLite initialized on the disk and used by the guest agent | 4.3 (+ 4.6 on real disk) |
| File written in the terminal persists across stop/start (demoable) | 4.5 (dev stack) + 4.6 (real FC, plus live-process survival) |
| `disk_id` and snapshot metadata recorded in Postgres | 4.2 (migration 000003, poller mapping) |
| Disk and snapshot files encrypted at rest; snapshots handled as secret material | decision #1/#2/#3; 4.0 (proof), 4.4 (impl), 4.6 (no-plaintext grep) |
| Resume reseeds guest entropy and resyncs the guest clock | decision #9; 4.0 (vmgenid findings), 4.3 (hook), 4.4 (call), 4.6 (assert) |

## Critical existing files to modify

- `nodeagent/internal/driver/firecracker/jail.go` — `prepareChroot` must stop deleting
  per-machine state (today it wipes the jail every boot); split scaffolding vs `/state`
- `nodeagent/internal/driver/firecracker/firecracker.go` — boot vs resume split in
  `bootOnce`; `finishStop` grows the hibernate path
- `nodeagent/internal/state/state.go` — Record fields; tap-name/IP stability is already
  persisted (resume depends on it — do not regress)
- `nodeagent/api/agentapi.go`, `nodeagent/internal/httpapi/` — request/response shapes, TLS
- `controlplane/internal/machine/state.go` — `allowed` map + transitional set gain `hibernating`
- `controlplane/internal/machine/{service,poller}.go` — key fetch, stop mode, status mapping
- `controlplane/internal/store/queries.sql` + migration `000003` + sqlc regen
- `controlplane/internal/nodeclient/nodeclient.go` — payloads + TLS pinning
- `guestagent/cmd/guestagent/main.go` — persist step before session manager
- `web/src/api/client.ts`, `web/src/components/MachineCard.tsx`
- `image/build-rootfs.sh` + `manifest.lock` — new guest agent baked in, ref re-pinned
- `spike/firecracker/README.md` — 09 row + Phase-4 findings; `RUNBOOK.md` — volumes dir,
  cryptsetup prereq, TLS cert provisioning

## Verification

- **Unit/integration (any OS):** `go test -race ./...` across all three modules — state
  machine additions, key redaction, Record round-trip, persist dir mode, SQLite, dev
  hibernate/resume, control-plane lifecycle e2e with snapshot-row assertions.
- **Manual e2e (Mac):** dev stack; write `~/proof.txt` in the terminal, Stop, watch
  `hibernating → stopped` over SSE, Start, file present, card shows "resumed".
- **Proxmox:** 4.0 spike green with findings recorded; 4.4 manual cycle; 4.6 gated
  `-tags firecracker` test incl. live-process survival, clock/entropy assertions, and the
  no-plaintext-at-rest grep; `cryptsetup status` / `findmnt` clean after stop.
- **CI:** migration applies in the existing Postgres job; sqlc diff clean; guestagent job
  covers persist+sqlite; gated KVM job runs 4.6.

## Non-goals / deferred

- **Disk backups / restore-from-backup** — Phase 12 (master plan explicitly accepts
  host-loss-loses-disks for the gated beta; the `disks` table is the seam).
- **Auto-hibernate on idle** — Phase 11 (the hibernate verb it needs ships here).
- **Secrets re-injection on resume** (env/agent keys) — Phase 5; the `PUT /resume` hook is
  deliberately the place it will hang off.
- **Network block storage / live migration** — behind the `Disk`/volume interface, per the
  master-plan disk-locality decision.
- **Per-user key rotation, OpenBao policies** — Phase 5 swaps the `secrets.Store` backend;
  Phase 10 hardens policy.
- **Snapshot retention/history (multiple snapshots)** — one current snapshot per machine;
  revisit if Phase 11 idle-hibernate wants generations.
- **Dev-driver session survival across hibernate** — impossible without RAM snapshot;
  documented; real path proven in 4.6.
