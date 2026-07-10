# ProteOS — Deployment (TAV-41)

How to deploy, roll forward, and roll back the production **app stack**
(`deploy/app-stack/`) using pre-built images from GHCR. For the full
first-time-setup walkthrough (node-agent, OpenBao init, Phase 5/6/8
acceptance) see **[RUNBOOK.md](RUNBOOK.md)**; for how the pieces fit together
see **[docs/architecture.md](docs/architecture.md)**. For "something is
broken right now," see **[INCIDENT_RUNBOOK.md](INCIDENT_RUNBOOK.md)**.

## Topology (short version)

Two machines — see RUNBOOK.md "Topology" for the full diagram:

- **App VM**: this Docker Compose stack (`postgres`, `openbao` +
  `bao-unsealer`, `controlplane`, `web`).
- **KVM host(s)**: the native `proteos-node-agent` (cannot be containerised —
  needs `/dev/kvm`, root, the jailer, host nftables). Not covered here; see
  `deploy/node-agent/` and RUNBOOK Part A.

## Images

`controlplane` and `web` no longer build from source at deploy time — they run
pre-built images published by `.github/workflows/ci.yml`'s `publish` job:

| Service       | Image                            | Built from                             |
| ------------- | --------------------------------- | --------------------------------------- |
| `controlplane`| `ghcr.io/tavon-ai/proteos-api`     | `deploy/app-stack/controlplane.Dockerfile` |
| `web`         | `ghcr.io/tavon-ai/proteos-ui`      | `deploy/app-stack/web.Dockerfile`        |

CI publishes on every push to `main` (tags: `latest` and the commit SHA, as
`sha-<short>` / `sha-<full>`) and on `v*.*.*` tags (semver: `1.2.3` and
`1.2`). Both images for a given commit are published together, so always pin
the **same** tag for both — that's what `PROTEOS_VERSION` in `.env` does:

```bash
# deploy/app-stack/.env
PROTEOS_VERSION=sha-a1b2c3d   # or a semver tag, e.g. 1.4.0
```

`compose.yaml` requires `PROTEOS_VERSION` to be set (`docker compose up`
fails loudly with a clear message if it's blank) and **refuses `latest`
only by convention, not enforcement** — don't use it in production: a
mutable tag means you can't tell what's actually running, and a rollback
has nothing concrete to roll back to. Pin a SHA or semver tag.

Pulling images needs read access to the GHCR packages. If they're private,
authenticate once per host:

```bash
echo "$GHCR_TOKEN" | docker login ghcr.io -u <github-user> --password-stdin
```

## First deploy

Follow RUNBOOK Part B as before, with one change: instead of
`docker compose up -d --build`, set `PROTEOS_VERSION` in `.env` first, then:

```bash
cd deploy/app-stack
cp .env.example .env && $EDITOR .env   # set PROTEOS_VERSION + the rest (RUNBOOK B2)
docker compose pull
docker compose up -d
docker compose logs -f controlplane   # "applying migrations" then "control plane listening"
```

## Ordinary deploy (postgres, openbao, web)

`postgres` and `openbao` are stateful, single-node, and rarely change image —
a normal recreate is fine and matches existing practice (RUNBOOK Part D).

`web` is a thin, stateless nginx (static SPA + reverse proxy) with no
in-process state worth preserving; recreating it is a sub-second blip and
long-lived terminal/editor WebSockets reconnect on their own once it's back
(RUNBOOK Part E5). Deploying it is a plain recreate:

```bash
docker compose pull web
docker compose up -d --no-deps web
```

## Rolling deploy of `controlplane` (avoids the hard restart)

Before this ticket, `docker compose up -d controlplane` stopped the *only*
running instance and started the new one in its place — every open terminal,
SSE stream, and in-flight API call dropped for the ~5-15s it takes the new
container to reach Postgres/OpenBao and start serving. `compose.yaml` now
carries `deploy.replicas: 1` for `controlplane` as its explicit steady state,
plus nginx (`nginx.conf`) resolves the `controlplane` upstream **dynamically**
(`resolver 127.0.0.11 valid=10s` + a variable in `proxy_pass`) instead of
caching it once at worker startup — both are prerequisites for the swap below
to actually route around whichever replica is mid-restart.

**Why not just set `replicas: 2` permanently?** `controlplane` keeps
in-process state that isn't shared across instances — see "Known
limitations" below. Two always-on replicas would silently duplicate
secret-injection pushes and drop live SSE/WebSocket updates for whichever
client is pinned to the "wrong" instance. Instead, scale to 2 **only for the
few seconds it takes to swap images**, then back to 1:

```bash
cd deploy/app-stack

# 1. Bump the pinned version and pull the new image.
$EDITOR .env                      # PROTEOS_VERSION=<new tag>
docker compose pull controlplane

# 2. Start a second controlplane container on the NEW image, alongside the
#    still-running old one. --scale overrides compose.yaml's replicas:1 for
#    this invocation only; the golang-migrate advisory lock makes it safe for
#    both to run `-migrate` concurrently (the second just waits, then no-ops).
docker compose up -d --no-deps --scale controlplane=2 controlplane

# 3. Wait for the NEW replica specifically to report ready (distroless image,
#    no shell/curl inside it — tail its own log, same signal RUNBOOK Part B3
#    already uses):
NEW=$(docker compose ps -q controlplane | tail -1)
docker logs -f "$NEW" | grep -q "control plane listening" && echo "new replica up"

# 4. Remove the OLD replica. nginx's resolver re-resolves `controlplane`
#    within 10s of this and stops sending it traffic; requests in flight on it
#    either finish or the client retries against the survivor.
OLD=$(docker compose ps -q controlplane | head -1)
docker rm -f "$OLD"

# 5. Scale back to the steady-state single replica (a no-op recreate check —
#    the one remaining container already matches compose.yaml).
docker compose up -d --no-deps --scale controlplane=1 controlplane
```

At every point in this sequence at least one `controlplane` instance is
serving requests — no hard restart. If step 3's log line never appears,
stop: do **not** remove the old replica; go to
[INCIDENT_RUNBOOK.md](INCIDENT_RUNBOOK.md).

If you don't need zero-downtime for a given deploy (e.g. a quiet maintenance
window), the simple path is still fine and faster:

```bash
docker compose pull controlplane
docker compose up -d --no-deps controlplane
```

## Rollback

Rollback is the same rolling-deploy procedure, run with `PROTEOS_VERSION` set
back to the last-known-good tag:

```bash
$EDITOR .env    # PROTEOS_VERSION=<previous tag>
docker compose pull controlplane web
docker compose up -d --no-deps --scale controlplane=2 controlplane
# ...steps 3-5 above, then:
docker compose up -d --no-deps web
```

**Finding the last-known-good tag.** `.env` is gitignored (it holds secrets),
so it isn't a version history by itself. Keep a short deploy log (even a
one-line append to a file or your team's chat channel: date, tag, who) each
time you change `PROTEOS_VERSION`. In a pinch, the tag is also recoverable
from what's already running:

```bash
# The image digest/tags of the currently-running container:
docker inspect $(docker compose ps -q controlplane) --format '{{.Config.Image}}'
# Every tag GHCR has for a given digest (needs GHCR API access):
docker inspect $(docker compose ps -q controlplane) --format '{{index .Config.Labels "org.opencontainers.image.revision"}}'
# ^ the commit SHA docker/metadata-action stamped as an image label — cross-
#   reference it against `git log` / the GHCR package's tag list.
```

**Database migrations are forward-only.** `golang-migrate` (via `-migrate`)
has no down-migration wired up in this project (`store.Migrate` only applies
forward). Rolling `PROTEOS_VERSION` back to a tag whose code predates a
schema change it now depends on will not un-apply that migration — the old
binary may then fail against the newer schema. In practice this is rarely an
issue (migrations are almost always additive), but if a rollback needs to
cross a migration boundary, treat it as an incident (schema fix-forward, not
a blind version rollback) rather than assuming rollback is symmetric with
deploy.

## Known limitations

- **`controlplane` steady state is 1 replica, not N.** The poller, the SSE
  machine-event broker, the task-events hub, the gateway WebSocket registry
  (session revocation), and the per-process rate limiter all hold state in
  that one process's memory — none of it is shared across instances. Running
  2+ replicas *permanently* would: duplicate secret-injection pushes on every
  `* → running` transition (idempotent, so not corrupting, just wasteful and
  noisy in audit logs); silently miss live SSE/WebSocket delivery for a
  client whose long-lived connection landed on the replica that *didn't*
  detect the change (falls back to the next reconnect, not truly lost); and
  multiply the effective rate limit by the replica count. The rolling-deploy
  swap above is safe because the overlap window is seconds, not permanent.
  Making N>1 replicas fully safe needs a shared broker (e.g. Postgres
  `LISTEN`/`NOTIFY` fan-out) and a shared rate limiter — not in scope here.
- **`PROTEOS_SECRETS_BACKEND=file` and the rolling swap don't mix.** The file
  secrets store writes `secrets.json` on the shared `cpdata` volume with no
  cross-process locking; two `controlplane` containers briefly both holding
  it open (step 2 above) is safe to *read*, but a concurrent *write* from
  both could corrupt it. Switch to `PROTEOS_SECRETS_BACKEND=openbao` (RUNBOOK
  Part D) before using the rolling-deploy/rollback swap in production; `file`
  is meant for a fresh dev stack, never production.
- **`web` is not replicated.** It publishes a fixed host port (`WEB_PORT`); a
  second replica can't bind the same port without a load balancer in front,
  which is out of scope here. It's cheap to recreate (see above), so this
  isn't the restart this ticket targets.
