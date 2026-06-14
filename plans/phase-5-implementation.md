# Phase 5 Implementation Plan: Secrets (OpenBao) + first AI agent (Claude Code)

> Source: `plans/proteos-poc-to-prod.md` Phase 5, planned 2026-06-11. Status: **not started.**
> Prerequisites: Phases 1–3 landed (secrets.Store stub + FileStore, machine lifecycle +
> poller, gateway + vsock tunnel + guest agent). **Phase 4 is planned but not landed**
> (`plans/phase-4-implementation.md`): Phase 5 is implementable in parallel on the Phase 3
> spine, but two master-plan criteria only close out after Phase 4 — "injected on every
> start **and resume**" (resume doesn't exist yet; the mechanism here covers it by
> construction) and durable `~/.claude` state (needs the persistent home). If Phase 4
> lands first, its machine volume keys move to OpenBao automatically via the same
> `secrets.Store` swap. Migration numbering below assumes Phase 4 takes `000003`; use the
> next free number if order flips.

Phase 5 closed.

## Context

Phase 1 deliberately shipped secrets behind a `Store` interface
(`controlplane/internal/secrets/secrets.go`) with a dev-only `FileStore` and the OpenBao
path conventions already canonical (`secret/users/<uid>/github`,
`secret/users/<uid>/providers/<key>`, `secret/machines/<id>/identity`). Phase 5 makes
secrets real and proves the first feature on top of them: a **real OpenBao backend with
per-user policies**, the **providers registry** (one row: Claude Code), **runtime
injection** of provider keys into the running VM, and **launching Claude Code** in a
terminal session that authenticates with the injected key. The demo: paste an Anthropic
API key into settings (never see it again), click "Launch Claude Code", get a working
Claude session in the browser.

Three facts shape the design:

1. **The control plane can already reach the guest directly** — `nodeclient.DialGuest`
   tunnels through the node-agent to the guest agent over vsock (Phase 3), with the
   node-agent as an opaque byte pipe. Secret injection can therefore be a **control-plane
   push** straight to the guest: the node-agent never parses or holds user secrets, and no
   guest-initiated (and therefore guest-authenticated) call is needed in this phase.
2. **PTY sessions already take an env overlay and a configurable executable**
   (`guestagent/internal/term/session.go:74,93`) — provider launch is a small extension of
   the session manager, not new machinery.
3. **The guest is untrusted** (master-plan trust model). The browser must never be able to
   choose what command runs or what env it gets; the registry (Postgres) decides, the
   control plane pushes, the guest only spawns commands it received from that push.

## Architecture decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | **OpenBao is a second `secrets.Store` implementation** (`secrets.BaoStore`, client `github.com/openbao/openbao/api/v2`), selected by `PROTEOS_SECRETS_BACKEND=file\|openbao` (default `file` — Mac dev stack unchanged). KV v2 mounted at `secret/`; the interface's `secret/users/...` paths map to KV v2 `secret/data/...` inside the client. Tested against a real OpenBao container via testcontainers (tech baseline: no mocks). | The Phase 1 interface was designed for exactly this drop-in; every existing caller (GitHub tokens, Phase 4 volume keys) moves to OpenBao by config, zero call-site changes. |
| 2 | **Per-user enforcement happens inside OpenBao, not just in Go**: the control plane's AppRole token cannot read user secrets at all. For any `secret/users/<uid>/...` operation, `BaoStore` derives the user from the path, idempotently ensures policy `user-<uid>` (scoped to `secret/data/users/<uid>/*` + matching `metadata`), mints a **short-lived (90 s) orphan child token** via token role `proteos-user` (`allowed_policies_glob: ["user-*"]`), and performs the op with that token. Machine paths (`secret/machines/*`) are covered directly by the control-plane policy. | Meets "OpenBao per-user policy restricts each user to their own secrets" honestly: a confused-deputy bug that builds user B's path while acting for user A fails **in Bao**, not in our code. Child tokens via a token role avoid per-user auth mounts/entities machinery; Phase 10 proves cross-user denial end-to-end. |
| 3 | **OpenBao deploys as a service in `deploy/app-stack/compose.yaml`** (single node, file storage backend, persistent volume) plus a documented one-time `openbao-init.sh`: `operator init` (operator records unseal keys), unseal, enable KV v2 at `secret/`, enable a file **audit device**, enable AppRole, create the `cp-base` policy + `proteos-user` token role + AppRole role, emit role_id/secret_id for the control-plane env. HA + auto-unseal are explicitly Phase 12 (master plan). A one-shot `controlplane -migrate-secrets` command copies the dev FileStore JSON into Bao for an existing deployment. | Matches the existing deploy shape (app stack on the app VM; Bao is reachable by the control plane only — it is already unreachable from VMs via the Phase 2 egress default-deny). Manual unseal is an accepted Phase 5–11 operational cost, called out in RUNBOOK. |
| 4 | **`providers` registry, migration `000004`**: `providers(key text PK, display_name text, launch_command text, secret_env jsonb, enabled bool, created_at)`. `secret_env` maps env var → field in the user's provider secret (claude: `{"ANTHROPIC_API_KEY":"api_key"}`). Seed row: `('claude','Claude Code','claude','{"ANTHROPIC_API_KEY":"api_key"}',true)`. The master plan's "image/template ref" column is deferred to Phase 6 — in the microVM architecture CLIs are baked into the rootfs, so Phase 5 needs only the launch command. | DB-backed from day one so Phase 6 is data + rootfs additions, not schema work. One seeded row keeps the registry honest (read by real code paths, not hardcoded). |
| 5 | **Secrets API**: `GET /api/providers` → `[{key, display_name, enabled, key_set}]`; `PUT /api/secrets/providers/{key}` body `{"api_key":"..."}` → validate registered+enabled, write to `secret/users/<uid>/providers/<key>`, 204, **never echoed**; `DELETE` removes. No read route exists at all. Redaction guarded by tests (key material absent from logs, responses, events — same discipline as Phase 4's volume key). React settings panel uses a write-only field and shows only `key_set`. | The acceptance criterion is "never appears in Postgres, logs, or the React app after submission" — the API shape makes the violation impossible rather than avoided. |
| 6 | **Audit slice (early Phase 10)**: migration `000004` also adds `audit_log(id bigserial PK, ts, user_id uuid NULL, actor text, action text, target text, metadata jsonb)` + a small `internal/audit.Recorder`. Rows written on: provider key put/delete (actor `user:<uid>`), injection reads (actor `system:injector`, action `secret.read`, target = path — never values), agent launches (`agent.launch`). | "Secret reads/writes are audited" lands in Postgres as the criterion asks; Bao's own audit device (decision #3) is belt-and-braces. Phase 10 extends the same table rather than inventing one. |
| 7 | **Injection = control-plane push over the existing tunnel.** New control-plane `internal/injector`: reads the user's provider secrets (per-user child token, decision #2), composes provider definitions from the registry, and calls a new guest endpoint `PUT /secrets` — plain HTTP over `nodeclient.DialGuest` (one-shot `http.Client` whose `DialContext` returns the tunnel conn; same trick the gateway uses for the guest WS). Triggered (a) by the poller on **every** `* → running` transition — which after Phase 4 includes resumes, satisfying "every start **and** resume" by construction — with retry/backoff, and (b) idempotently before any agent launch. The guest agent stores definitions in memory and writes env files to `/run/proteos/env/<key>.env` (tmpfs, 0600): never on the rootfs image or the persistent disk. | Push keeps the node-agent an opaque pipe and needs no guest-side credentials. tmpfs contents end up in snapshots — the master plan accepts this explicitly ("snapshots contain guest RAM, including any secrets injected at start") and Phase 4 encrypts snapshots at rest; re-push on every resume keeps them fresh. |
| 8 | **Per-machine identity (`secret/machines/<id>/identity`) is deferred to Phase 7**, deviating from Phase 3 decision #10's "Phase 5" note. Update that comment + guestagent README. | It exists to authenticate **guest-initiated** calls; Phase 5 has none (push model). Phase 7's git credential helper is the first real consumer — minting an identity nothing consumes would be dead, untested machinery. |
| 9 | **Launching Claude, two surfaces.** (a) Plain terminal: a rootfs `profile.d` snippet sources `/run/proteos/env/*.env` into login shells (sessions run `<shell> -l`), so typing `claude` in any new terminal just works. (b) **`WS /gw/agent/{provider}`** (the durable route shape): gateway reuses the Phase 3 terminal handler chain (auth → Origin → ownership → running) plus provider checks (registered+enabled else 404, `key_set` else `409 no_provider_key`), idempotent push, then dials guest `/terminal?session=agent-<key>`. Guest agent: session names with the `agent-` prefix spawn that provider's **injected** launch command with its env overlay instead of the shell; unknown/uninjected provider → WS close with a clear code. The browser never transmits a command or env — only the provider key, validated against the registry. | Reuses the entire Phase 3 session model: agent sessions get tmux-like semantics for free (drop WS, reconnect, scrollback — a long Claude run survives a browser reload). One agent session per provider per machine this phase; multiples are Phase 9. The untrusted-browser/untrusted-guest boundary stays clean (fact #3). |
| 10 | **Claude Code is baked into the rootfs at a pinned version** by `image/build-rootfs.sh` (binary install into `/usr/local/bin/claude`, version + sha256 recorded in `manifest.lock`; image version bump + re-pin `PROTEOS_ROOTFS_REF`). First-run friction (Claude Code interactively confirms use of `ANTHROPIC_API_KEY`) is pre-answered in the image via its settings mechanism — verified in the 5.7 acceptance pass, since this is exactly the kind of CLI detail that drifts. Per-user state (`~/.claude*`) lands in `$HOME` → durable once Phase 4's persistent home exists. | Matches the version-pinned-image philosophy (and Phase 12's pipeline seed). Installing at first launch inside each VM would be slow, flaky, and unpinned. |

## Wire contracts

### OpenBao layout (created by `openbao-init.sh` / `BaoStore`)

```
mounts:    secret/ (KV v2), audit device: file
approle:   role proteos-cp → policy cp-base:
             path "auth/token/create/proteos-user"        { capabilities = ["update"] }
             path "sys/policies/acl/user-*"               { ["create","update","read"] }
             path "secret/data/machines/*"                { ["create","update","read","delete"] }
             path "secret/metadata/machines/*"            { ["read","delete","list"] }
token role proteos-user: allowed_policies_glob=["user-*"], orphan, ttl=90s, renewable=false
per-user (lazily ensured): policy user-<uid>:
             path "secret/data/users/<uid>/*"             { ["create","update","read","delete"] }
             path "secret/metadata/users/<uid>/*"         { ["read","delete","list"] }
```

### Control-plane API (new)

```
GET    /api/providers                      → 200 [{"key":"claude","display_name":"Claude Code",
                                                   "enabled":true,"key_set":true}]
PUT    /api/secrets/providers/{key}        body {"api_key":"sk-…"}   → 204 (write-only; 404 unknown
                                             provider; 422 empty/oversized key)
DELETE /api/secrets/providers/{key}        → 204
WS     /gw/agent/{provider}                → agent terminal session "agent-<key>" (guestwire protocol,
                                             same close codes as /gw/terminal, plus pre-upgrade
                                             404 unknown_provider · 409 no_provider_key)
```

### Guest agent (guestwire additions)

```
PUT /secrets   {"providers":{"claude":{"command":"claude",
                "env":{"ANTHROPIC_API_KEY":"sk-…"}}}}        → 204
  semantics: replace-all, idempotent; in-memory map + /run/proteos/env/<key>.env (0600, tmpfs)
GET /terminal?session=agent-<key>   → spawns the injected provider command (env overlay) instead
                                      of the shell; no injected definition → close 4003 provider_unavailable
```

### Config additions

```
controlplane: PROTEOS_SECRETS_BACKEND=file|openbao (default file),
              PROTEOS_OPENBAO_ADDR, PROTEOS_OPENBAO_MOUNT=secret,
              PROTEOS_OPENBAO_ROLE_ID, PROTEOS_OPENBAO_SECRET_ID_FILE
deploy:       openbao service + volume in deploy/app-stack/compose.yaml; openbao-init.sh
cli:          controlplane -migrate-secrets <filestore.json>   # one-shot FileStore → Bao copy
```

## Package layout (new / touched)

```
controlplane/
  internal/secrets/bao.go                  # BaoStore: KV v2 mapping, ensurePolicy cache, child tokens
  internal/secrets/bao_test.go             # testcontainers OpenBao (real container, no mocks)
  internal/audit/audit.go                  # Recorder → audit_log rows
  internal/providers/providers.go          # registry reads, key_set composition
  internal/injector/injector.go            # Bao read → PUT /secrets over DialGuest; retry/backoff
  internal/httpapi/providers.go            # GET /api/providers, PUT/DELETE /api/secrets/providers/{key}
  internal/gateway/agent.go                # /gw/agent/{provider}: provider checks + push + dial
  migrations/000004_providers_audit.*.sql  # providers (seeded) + audit_log
  cmd/controlplane/main.go                 # backend selection, -migrate-secrets, injector wiring
guestagent/
  internal/secrets/secrets.go              # in-memory provider defs + /run/proteos/env files
  internal/server/                         # PUT /secrets; agent- session name → provider spawn
  internal/term/                           # Session: optional command + env overlay (tiny extension)
image/
  build-rootfs.sh                          # + pinned Claude Code install, profile.d snippet, manifest
  profile.d-proteos-providers.sh           # sources /run/proteos/env/*.env in login shells
deploy/app-stack/{compose.yaml,openbao-init.sh}
web/src/
  components/ProvidersPanel.tsx            # key status, write-only set/replace, launch button
  api/client.ts, components/TerminalPanel  # provider types; agent-session terminal reuse
```

## Tasks (Track A = Mac/dev-driver; Track B = Proxmox/Firecracker)

### 5.0 — `BaoStore` + per-user policies (Track A; standalone)
`internal/secrets/bao.go` per decisions #1/#2: KV v2 path mapping, lazy `ensurePolicy`
(in-process cache), child-token-per-user-op, machine paths on the base token; config
backend selection; `-migrate-secrets`. Tests against a real OpenBao testcontainer:
round-trips, `ErrNotFound` semantics identical to FileStore, **cross-user denial proven
in Bao** (token for user A physically cannot read user B's path), policy idempotency.
**Done when:** the whole existing suite passes with `PROTEOS_SECRETS_BACKEND=openbao`
against the container (GitHub-token flow included) and the denial test is green.

### 5.1 — OpenBao in the app stack (Track A; after 5.0)
Compose service + volume, `openbao-init.sh` per decision #3 (idempotent re-run safe),
RUNBOOK section (init, unseal, AppRole creds into `.env`, audit device, backup note),
secret-migration runbook step.
**Done when:** a fresh `docker compose up` + init script yields a control plane serving
logins with secrets in Bao; restart-with-unseal documented and exercised.

### 5.2 — providers registry + secrets API + audit slice (Track A; after 5.0)
Migration `000004` (+ sqlc regen), `internal/providers`, `internal/audit`, httpapi routes
per decision #5 (requireAuth, validation, redaction), audit rows per decision #6;
`key_set` via Bao read with the user-scoped token (value discarded, never logged).
**Done when:** handler tests cover the route table (404/422/204), `audit_log` rows assert
on put/delete, and a log/response-scan test proves the key never escapes.

### 5.3 — guest agent: secret store + provider sessions (Track A; parallel with 5.2)
`internal/secrets` (replace-all semantics, env files on tmpfs, 0600), `PUT /secrets`
route, `term.Session` optional command+env (exists as `cfg.Env` — wire it through
`Manager`), `agent-<key>` session-name dispatch, close code 4003 `provider_unavailable`.
**Done when:** unit tests prove: push → env file content + perms; `agent-claude` session
spawns a stub command that sees `ANTHROPIC_API_KEY`; uninjected provider closes 4003;
re-push replaces definitions; plain shell sessions are untouched.

### 5.4 — injector + `/gw/agent/{provider}` (Track A; after 5.2 + 5.3)
`internal/injector` per decision #7 (HTTP-over-tunnel client, poller `toRunning` hook,
retry/backoff, audit `secret.read` rows); gateway route per decision #9 (provider
checks → idempotent push → dial `session=agent-<key>`); `agent.launch` audit row.
**Done when:** e2e on the 3.4 harness (httptest control plane + real node-agent DevDriver
+ real guest agent + Bao testcontainer): set key → machine reaches running → env file
appears in the guest; `/gw/agent/claude` round-trips a stub-claude session; no-key →
409; unknown provider → 404; key absent from all logs/events.

### 5.5 — rootfs: Claude Code + profile.d (Track B; parallel after 5.3)
`build-rootfs.sh`: pinned Claude Code binary install (version + sha256 into
`manifest.lock`), first-run API-key approval pre-answered in the image, profile.d
snippet; rebuild, re-pin `PROTEOS_ROOTFS_REF`, stage on the Proxmox host.
**Done when:** a VM booted from the new image has `claude --version` working as root and
a login shell sources `/run/proteos/env/*.env`.

### 5.6 — React: providers settings + launch (Track A; after 5.2, launch wiring after 5.4)
`ProvidersPanel` (list, key_set badge, write-only set/replace/delete with confirm),
"Launch Claude Code" on the Dashboard/MachineCard when `running` + `key_set`, opening a
`TerminalPanel` against `/gw/agent/claude` (Terminal component gains a URL/session prop);
client types.
**Done when:** Mac dev stack: paste key (never rendered back), launch button opens a
stub-claude agent session; missing-key and stopped-machine states render clear CTAs.

### 5.7 — live acceptance pass (Track B; after 5.4 + 5.5 + 5.6)
On the Proxmox stack with OpenBao deployed (5.1): real Anthropic key via settings →
launch Claude Code → prompt it to write a file in the workspace; verify key in Bao
(`bao kv get` as operator), absent from Postgres dumps + control-plane/node-agent logs;
stop/start machine → key re-injected (cold-boot path; **re-run the resume leg after
Phase 4 lands** to tick "and resume"); audit rows present for put/read/launch; browser
reload mid-Claude-session reattaches with scrollback. Walk the master-plan Phase 5
checklist and tick the boxes in `plans/proteos-poc-to-prod.md`.

### Sequencing

```
5.0 ──► 5.1 (deploy)
  └───► 5.2 ──┬──► 5.4 ──┬──► 5.6 ──► 5.7 (Track B)
       5.3 ───┘          │
       5.3 ──► 5.5 (Track B rootfs) ─┘
Buildable immediately in parallel: 5.0, 5.3. The only Track B work is 5.5 + 5.7.
```

## Acceptance-criteria mapping (master-plan Phase 5 checklist)

| Criterion | Task |
|---|---|
| `PUT /api/secrets/providers/claude` → OpenBao under the user's path; never in Postgres/logs/React | 5.2 (route + redaction tests), 5.0 (storage), 5.6 (write-only UI), 5.7 (live verify) |
| OpenBao per-user policy restricts each user to their own secrets | 5.0 (decision #2 + denial test in Bao) |
| On machine start, the Claude key is injected into the running VM at runtime | 5.4 (poller hook + push), 5.7 (live) |
| `providers` table exists; Claude Code registered and shown as available | 5.2 (migration + seed), 5.6 (UI) |
| User launches Claude Code in a terminal; it authenticates with the injected key | 5.3 + 5.4 + 5.5, demoed in 5.7 |
| Injected on every start **and resume**; secret reads/writes audited | 5.4 (transition-hook construction + audit slice); resume leg re-verified post-Phase 4 (5.7 note) |

## Critical existing files to modify

- `controlplane/cmd/controlplane/main.go` — backend selection (today hardcodes
  `NewFileStore`, line 63), injector + audit wiring, `-migrate-secrets`
- `controlplane/internal/config/config.go` — Bao env vars
- `controlplane/internal/machine/poller.go` — `toRunning` gains the injection hook
  (keep it non-blocking; lifecycle must not depend on Bao availability)
- `controlplane/internal/httpapi/server.go` — providers/secrets routes; `/gw/agent/{provider}`
- `controlplane/internal/store/queries.sql` + migration `000004` + sqlc regen
- `controlplane/internal/gateway/` — agent route reusing the terminal serve path
- `guestagent/internal/server/server.go` — `PUT /secrets`; `agent-` session dispatch
- `guestagent/internal/term/{manager,session}.go` — command + env overlay through `Manager`
- `guestagent/api` (guestwire) — secrets payload types, close code 4003
- `image/build-rootfs.sh` + `manifest.lock` — Claude Code + profile.d; ref re-pin
- `deploy/app-stack/compose.yaml`, `RUNBOOK.md` — OpenBao service, init/unseal/migration ops
- `web/src/api/client.ts`, `Dashboard.tsx`, `MachineCard.tsx`
- Phase 3 decision-#10 comment at the guestagent listener + README — identity now Phase 7
  (decision #8)

## Verification

- **Unit/integration (any OS):** `go test -race ./...` — BaoStore against a real OpenBao
  testcontainer incl. cross-user denial; providers/secrets route table; redaction scans;
  guest secret store + provider session spawn; audit-row assertions.
- **e2e (Mac, normal CI):** the 5.4 harness — key set → injection on running → agent
  session env-asserted via stub command → 404/409 negative paths.
- **Live (Proxmox):** 5.1 init + 5.7 walkthrough with a real Anthropic key — launch,
  prompt, file written; key absent from PG dump and all logs; stop/start re-injection;
  reload-reattach mid-session.
- **CI:** migration 000004 + sqlc diff in the existing Postgres job; OpenBao
  testcontainer runs on standard runners (no KVM needed); web typecheck/build.

## Non-goals / deferred

- **Gemini / OpenAI / pi.dev providers** — Phase 6 (this phase proves the registry shape
  with one row; Phase 6 is data + rootfs additions).
- **Per-machine identity + guest-initiated secret fetches** — Phase 7 (decision #8); the
  push model needs neither.
- **OpenBao HA, auto-unseal, sealed-state alerting** — Phase 12 (master plan).
- **Secret rotation UX, key validation against the Anthropic API** — later; Phase 5
  validates shape/size only (a bad key surfaces in the Claude session itself).
- **Full audit coverage + policy-denial testing end-to-end** — Phase 10 (this ships the
  table + first writers).
- **Multiple concurrent agent sessions per provider, agent windowing** — Phase 9
  (session-name scheme `agent-<key>` already leaves room: Phase 9 adds suffixes).
- **Rate-limiting the secrets routes** — Phase 10 (noted there already).
