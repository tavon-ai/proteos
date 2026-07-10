# ProteOS — Incident Runbook (TAV-41)

What to do when the production app stack is misbehaving *right now*. For
routine deploys/rollbacks see **[DEPLOYMENT.md](DEPLOYMENT.md)**; for
first-time setup and acceptance walkthroughs see **[RUNBOOK.md](RUNBOOK.md)**
(its "Gotchas" sections have the deep technical detail behind several of the
symptoms below — this doc is the triage flow, RUNBOOK.md is the reference).

## Severity

- **SEV1 — full outage.** Nobody can sign in, load the dashboard, or reach any
  machine. Page immediately; consider rollback (below) before root-causing.
- **SEV2 — degraded.** Sign-in/dashboard works but some feature is broken for
  everyone (e.g. Create fails, all terminals disconnect and won't
  reconnect, secrets injection failing) or for a subset of users.
  Investigate first; roll back if the cause traces to the last deploy and a
  fix isn't quick.
- **SEV3 — isolated.** One user or one machine misbehaving; others unaffected.
  Handle during business hours unless it's actively spreading.

## First 5 minutes (any severity)

1. **Is this right after a deploy?** `docker compose ps` /
   `docker inspect <container> --format '{{.Config.Image}}'` on the app VM —
   compare the running `controlplane`/`web` tag against your deploy log. If
   yes and the timing lines up, **stop investigating and roll back** (skip to
   [Rollback](#rollback-a-bad-deploy) below) — restoring service comes before
   understanding why.
2. **Scope it.** Open the dashboard yourself. Full outage vs. one feature vs.
   one machine changes the severity and the playbook below.
3. **Check the obvious layer first, in order**: `web` (nginx) →
   `controlplane` → `postgres` → `openbao` → node-agent. Each of the sections
   below assumes you've narrowed it to one.

   ```bash
   cd deploy/app-stack
   docker compose ps                       # everything "running (healthy)"?
   docker compose logs --tail=200 web controlplane postgres openbao bao-unsealer
   ```

## Rollback a bad deploy

Follow **[DEPLOYMENT.md § Rollback](DEPLOYMENT.md#rollback)**: set
`PROTEOS_VERSION` back to the last-known-good tag and re-run the rolling-swap
procedure. This is almost always faster than root-causing a bad release live.
Do this **before** deep-diving the "why" for a SEV1 caused by a deploy —
restore service, then investigate the bad image/commit offline.

If the rollback itself doesn't fix it, the deploy wasn't the cause — move to
the relevant section below.

## Common incidents

### `controlplane` won't come up / crash-loops

```bash
docker compose logs --tail=100 controlplane
```

- **`applying migrations` then nothing / an error** — a migration failed.
  `golang-migrate` leaves the schema mid-migration; do not just restart
  repeatedly. Check the error, fix the migration, or restore Postgres from
  backup if the schema is now inconsistent. Do not attempt a code rollback
  across the migration boundary (DEPLOYMENT.md's "migrations are forward-only"
  caveat) — that makes it worse, not better.
- **`secrets self-check denied: ...`** — `PROTEOS_OPENBAO_PREFIX` doesn't
  match the `cp-base` policy openbao-init.sh granted. Fix the env or re-run
  `openbao-init.sh` with the matching prefix (RUNBOOK Part D2), then
  `docker compose up -d --force-recreate controlplane`.
- **`secrets backend self-check failed`** (not a permission-denied) — OpenBao
  itself is unreachable or sealed. See "OpenBao sealed" below first.
- **Exits immediately with a config error** (`PROTEOS_BASE_URL`,
  `GITHUB_APP_CLIENT_ID`, `PROTEOS_AGENT_TOKEN`, etc. `:?` messages) — a
  required env var is missing from `.env`. Check what changed; this should
  only happen after an `.env` edit, never a plain image bump.
- **Container starts but the log never reaches `control plane listening`** —
  it's blocked on Postgres or OpenBao; check those next.

### OpenBao sealed / secrets down

```bash
docker compose exec openbao bao status         # Sealed: true?
docker compose logs bao-unsealer | tail -20     # is it retrying? erroring?
```

- OpenBao boots sealed by design; `bao-unsealer` should unseal it within ~10s
  using `./bao-unseal-key`. If it's been sealed longer than that, the sidecar
  is missing or rejecting its key file — see RUNBOOK Part D4 for the manual
  unseal + `openbao-init.sh` re-run to repopulate `bao-unseal-key`.
- **Impact while sealed**: `controlplane` fails its secrets self-check and
  won't start fresh (existing running instances keep serving with whatever
  they already loaded, but can't read/write new secrets — provider key
  changes and new-machine secret injection fail).
- If `openbao-init.json` / `bao-unseal-key` are both lost (e.g. volume wiped
  without a backup), the data in `baodata` is **unrecoverable** — this is a
  full secrets-loss incident, not a quick fix. Restore both files + the
  `baodata` volume from backup together (RUNBOOK Part D5) — restoring one
  without the other is useless. If there is no backup, every user must
  re-enter their provider API keys and reconnect GitHub.

### Postgres down / unreachable

```bash
docker compose ps postgres               # healthy?
docker compose logs --tail=100 postgres
```

- `controlplane` depends on `postgres: condition: service_healthy` and won't
  even start without it — if `controlplane` is crash-looping, check this
  first, not last.
- Disk full on the `pgdata` volume is the most common real-world cause of a
  healthy-looking Postgres that starts silently rejecting writes; check host
  disk space (`df -h`) before assuming an application bug.
- Don't `docker compose down -v` to "reset" Postgres in an incident — that
  drops the volume (and every machine's DB state) permanently. If Postgres
  itself is corrupted, restore from your DB backup instead.

### All terminals/editors dropped and won't reconnect

- A single dropped WebSocket that reconnects on its own is normal (nginx/
  controlplane restart, RUNBOOK Part E5). "Won't reconnect" points at the
  gateway or the node-agent link, not a blip.
- Check `controlplane` can still reach the node-agent:
  ```bash
  docker compose logs controlplane | grep -i "node-agent\|agent unreachable" | tail -20
  ```
- Confirm the shared bearer token still matches on both sides
  (`PROTEOS_AGENT_TOKEN` in `.env` vs. the node-agent's own config) — a
  mismatch 401s every agent call and machines flip to `error` (RUNBOOK Part
  C7 describes this exact failure shape).
- If the node-agent host itself is down/unreachable (network, host reboot,
  KVM host issue), that's a node-agent incident, not a control-plane one —
  existing microVMs keep running on that host, but the control plane can't
  supervise or reach them until it's back.

### Editor/preview subdomains return the dashboard HTML instead of the editor

This is a known nginx-config gotcha, not a runtime failure — see RUNBOOK Part
G5. It means the wildcard `server_name ~^m-...` block in `nginx.conf` (or the
proxy layer's WebSocket toggle) is missing from what's actually deployed —
check the running `web` image is the one you think it is (`DEPLOYMENT.md`
"Finding the last-known-good tag").

### Elevated error rates, no obvious single cause

```bash
docker compose logs controlplane --since 15m | grep -c '"level":"error"'
```

Set `PROTEOS_LOG_LEVEL=debug` (`docker compose up -d controlplane` after
editing `.env`) if the errors read as `"internal"` — the control plane always
logs the underlying cause at `error`, but debug surfaces more context around
it. Revert to `info` once diagnosed; leaving it at `debug` in production is
noisy but not unsafe.

## After the incident

- If you rolled back, the underlying bug in the newer image is still there —
  file it before re-attempting that deploy.
- Update this runbook if you hit a failure mode it didn't cover, or if a
  documented fix was wrong/incomplete.
- For anything that touched secrets, cross-tenant isolation, or the guest
  sandbox boundary, follow **[SECURITY.md](SECURITY.md)** regardless of
  whether it was reported externally.
