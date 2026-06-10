# Plan: ProteOS — PoC to Production

> Source: in-conversation vision (2026-06-10). Turn the ProteOS PoC into a production
> system: a remote per-user "machine" accessed via browser where users work on their
> projects using AI coding agents.

## Context: what exists today (PoC)

- Single-file Express server (`server/index.js`) using `dockerode` to build per-provider
  Docker images (`dockerfile.claude/gemini/openai`), each running a CLI in a `ttyd` web
  terminal on container port `7681`.
- Vanilla-JS windowing desktop (`public/app.js`) that embeds `<iframe>`s pointing
  **directly** at each container's `ttyd` (`http://<host>:<port>`).
- In-memory `containers` Map (no persistence; state lost on restart), **no auth**,
  Docker-in-Docker via mounted socket, hardcoded socket fallback path.

This plan replaces essentially all of it. The PoC is the reference for *behavior*
(provider registration pattern, terminal-in-browser, windowing desktop), not for code.

---

## Architectural decisions

Durable decisions that apply across all phases. Phases reference these rather than
re-specifying them.

### Topology

```
React SPA
   │  HTTPS / WSS (authenticated)
   ▼
Go control plane ── Postgres (system of record)
   │  ├─ auth / sessions / GitHub OAuth
   │  ├─ machine lifecycle + scheduler
   │  ├─ reverse-proxy / WS gateway  ──► guest agent (per VM)
   │  └─ OpenBao client (secrets)
   ▼  (schedules onto)
node-agent (one per host)
   │  drives Firecracker (firecracker-containerd / Firecracker API)
   ▼
microVM (per user, persistent)
   ├─ own kernel + rootfs
   ├─ persistent disk: /home + workspace + machine SQLite
   └─ in-VM guest agent: terminal, code-server, AI agent CLIs, git
```

### Components

- **Control plane (Go)**: stateless-ish API + auth + scheduler + secrets broker +
  the **gateway** that proxies browser↔VM traffic. Horizontally scalable behind a LB.
- **node-agent (Go)**: privileged daemon on each compute host. Owns Firecracker VMM
  processes, tap devices, disk attach/snapshot. Talks to the control plane over an
  internal authenticated channel. The only component that touches the hypervisor.
- **guest agent (in-VM)**: small daemon inside each microVM. Exposes terminal (PTY over
  WS), launches agent CLIs, serves code-server, runs git, owns the machine SQLite.
  Reachable **only** through the gateway over the private per-tenant network.

### Networking / reaching the VM

- Each microVM gets a **tap device + private IP** on a per-tenant/host bridge. No VM
  port is ever exposed publicly (this is the key departure from the PoC's direct
  `iframe → :7681`).
- All browser↔VM traffic (terminal WS, code-server HTTP/WS, agent I/O) is brokered by
  the **control-plane gateway**, which authorizes the session, looks up the user's VM,
  and proxies to the guest agent's private address.

### Machine lifecycle (state machine)

`requested → provisioning → running → hibernating → stopped → error`

- **stop = snapshot/hibernate** (Firecracker snapshot of memory + pause), `start = resume`.
- `stopped` may also mean cold (no snapshot) for cost; disk always persists.
- All transitions emit a row in `machine_events` (audit + reconciliation source).

### Data model

**Postgres (system of record):**
- `users` — id, github_user_id, login, email, created_at, status.
- `sessions` — server-side session/token, user_id, expiry.
- `machines` — id, user_id (1:1 for now), state, host_id, vm_handle, disk_id,
  resource_spec, last_active_at, created_at. (One persistent machine per user.)
- `machine_events` — machine_id, type, from_state, to_state, actor, payload, ts.
- `providers` — agent provider registry: key (`claude|gemini|openai|pi`), display name,
  image/template ref, required secret keys, enabled.
- `hosts` — compute node inventory: id, capacity, allocatable, status (Phase 11).
- `github_links` — user_id, scopes, openbao_ref (token never stored in PG).

**SQLite (on each machine's persistent disk):** window layout, open agent sessions,
per-machine preferences, project/repo metadata, terminal scrollback index. Purely
machine-local; never authoritative for billing/identity.

### Secrets (OpenBao)

- Path convention: `secret/users/<user_id>/github`, `secret/users/<user_id>/providers/<key>`,
  `secret/machines/<machine_id>/identity`.
- Per-user OpenBao **policy**; control plane uses short-lived tokens/AppRole; node-agent
  and guest receive only the minimum, **injected at VM start**, never baked into images.
- Secrets never transit through the React app or land in Postgres.

### Auth

- **GitHub OAuth** (web flow) is primary identity. Request `repo` (or finer-grained)
  scope so the same grant powers git operations.
- Server-side session in Postgres + httpOnly, Secure, SameSite cookie. CSRF protection
  on state-changing routes.
- GitHub access/refresh tokens stored in **OpenBao**, referenced from `github_links`.

### API surface (durable route shapes)

```
GET  /api/auth/github/login          → redirect to GitHub
GET  /api/auth/github/callback       → exchange code, create session
POST /api/auth/logout
GET  /api/me                         → current user + machine summary

GET  /api/machine                    → my machine (state, spec)
POST /api/machine                    → create my machine
POST /api/machine/start              → resume/boot
POST /api/machine/stop               → hibernate/stop
GET  /api/machine/events             → lifecycle/audit stream (SSE/WS)

WS   /gw/terminal                    → PTY to guest agent (proxied)
ANY  /gw/code-server/*               → code-server (proxied)
ANY  /gw/agent/<provider>           → AI agent session (proxied)

GET  /api/providers                  → enabled agent providers
PUT  /api/secrets/providers/<key>    → set provider API key (→ OpenBao)
GET  /api/git/repos                  → user repos (via GitHub)
POST /api/git/clone                  → clone repo into machine
```

### Tech baseline

- **Frontend**: React (SPA), TypeScript, a router, a data-fetching layer; xterm.js for
  terminals; code-server embedded via the gateway.
- **Backend**: Go; `pgx`/`sqlc` for Postgres; standard `net/http` reverse proxy + a WS
  proxy for the gateway; Firecracker via `firecracker-containerd` or the Firecracker SDK.
- **Infra**: Postgres, OpenBao, compute hosts with KVM for Firecracker.

### Cross-cutting non-goals for early phases (added in the hardening tail)

Multi-host scheduling, auto-hibernate on idle, quotas, full audit/observability, backups,
and CI/CD are deliberately deferred to Phases 10–12 so the spine ships first.

---

## Phase 1: Control-plane skeleton + GitHub auth

**User stories**: As a user, I can sign in with GitHub and land on my dashboard, which
shows I don't have a machine yet.

### What to build

A new Go control plane and a React SPA, wired to Postgres, with the complete GitHub
OAuth login flow and server-side sessions. No compute yet — the dashboard reads a
`machines` row that doesn't exist and shows an empty state. This establishes the
repo structure, config, migrations, auth, and the React↔Go contract that every later
phase builds on.

### Acceptance criteria

- [ ] `users` and `sessions` tables exist via migrations; Postgres is reachable from Go.
- [ ] GitHub OAuth login + callback works; a session cookie is issued; `GET /api/me`
      returns the authenticated user.
- [ ] Logout clears the session.
- [ ] React app shows a login screen when unauthenticated and a dashboard (with
      "no machine yet" empty state) when authenticated.
- [ ] GitHub tokens are written to OpenBao (or a clearly-stubbed secrets interface if
      OpenBao lands in Phase 5) — **not** to Postgres.
- [ ] Unauthenticated access to `/api/machine*` is rejected.

---

## Phase 2: Provision a Firecracker microVM (lifecycle)

**User stories**: As a user, I can create my machine and start/stop it, and see its
current state update in the dashboard.

### What to build

The node-agent and the control-plane lifecycle. The control plane schedules a machine
onto a (single, hardcoded-for-now) host; the node-agent boots a Firecracker microVM from
a base kernel + rootfs, sets up a tap device + private IP, and reports status. The
machine state machine and `machine_events` are implemented. No terminal yet — success is
verified through machine state and host-side checks.

### Acceptance criteria

- [ ] `machines`, `machine_events`, `hosts` (minimal) tables exist.
- [ ] `POST /api/machine` creates a record (`provisioning`) and the node-agent boots a
      real Firecracker microVM that reaches `running`.
- [ ] `POST /api/machine/stop` and `/start` transition the VM and persist state.
- [ ] Each VM gets a tap device + private IP; the control plane records `vm_handle`/host.
- [ ] Every transition writes a `machine_events` row.
- [ ] Dashboard reflects live machine state; `GET /api/machine/events` streams updates.
- [ ] Node-agent ↔ control-plane channel is authenticated (not open on the network).

---

## Phase 3: Browser terminal through the gateway

**User stories**: As a user, I can open an interactive terminal to my machine in the
browser.

### What to build

The in-VM guest agent (PTY over WS) and the control-plane **gateway** that authorizes a
session, resolves the user's VM private address, and proxies the terminal WebSocket. The
React app renders the terminal with xterm.js. This proves the end-to-end
browser→gateway→guest path that code-server and agents will reuse.

### Acceptance criteria

- [ ] Guest agent runs inside the microVM and exposes a PTY over WS on the private network.
- [ ] `WS /gw/terminal` authenticates the user, authorizes ownership of the target VM,
      and proxies to the guest — **no VM port is publicly reachable**.
- [ ] React terminal is interactive (input/output, resize) against a real shell in the VM.
- [ ] Terminal access is denied for users who don't own the machine / aren't logged in.
- [ ] Disconnect/reconnect behaves sanely (session cleanup on the gateway).

---

## Phase 4: Persistent disk + hibernate/resume

**User stories**: As a user, my files and machine state survive stopping and starting my
machine.

### What to build

A per-user persistent block device (home + workspace) attached to the microVM, plus
snapshot-based hibernate on stop and resume on start. The machine-local **SQLite** is
created and lives on this disk. Data durability across the full lifecycle is the
deliverable.

### Acceptance criteria

- [ ] A persistent disk is provisioned per machine and attached at boot; survives
      stop/start and host process restarts.
- [ ] `stop` snapshots/hibernates the VM; `start` resumes (or cold-boots) with the disk
      reattached and state intact.
- [ ] Machine SQLite is initialized on the disk and used by the guest agent.
- [ ] A file written in the terminal persists across a stop/start cycle (demoable).
- [ ] `disk_id` and snapshot metadata are recorded in Postgres.

---

## Phase 5: Secrets (OpenBao) + first AI agent (Claude Code)

**User stories**: As a user, I can securely store my Anthropic API key and run Claude
Code inside my machine.

### What to build

Full OpenBao integration: per-user policies, the secrets-broker in the control plane, and
runtime injection of secrets into the VM (env/file inside the guest, never in the image).
The `providers` registry is introduced with one entry (Claude Code); the guest agent can
launch the Claude CLI in a terminal session using the injected key.

### Acceptance criteria

- [ ] `PUT /api/secrets/providers/claude` stores the key in OpenBao under the user's path;
      it never appears in Postgres, logs, or the React app after submission.
- [ ] OpenBao per-user policy restricts each user to their own secrets.
- [ ] On machine start, the Claude key is injected into the running VM at runtime.
- [ ] `providers` table exists; Claude Code is registered and shown as available.
- [ ] User can launch Claude Code in a terminal in the machine and it authenticates with
      the injected key.

---

## Phase 6: Provider registry + remaining agents (Gemini, OpenAI, pi.dev)

**User stories**: As a user, I can choose and launch any of the four AI coding agents
(Claude, Gemini, OpenAI Codex, pi.dev).

### What to build

Generalize the provider mechanism so adding an agent is data + a base template, not bespoke
code. Add Gemini CLI, OpenAI Codex, and **pi.dev** as registered providers, each with its
required secret keys and launch command. React UI lists enabled providers and launches any
of them via the gateway.

### Acceptance criteria

- [ ] `providers` registry drives the available-agent list (DB-backed, no hardcoding).
- [ ] All four providers (`claude`, `gemini`, `openai`, `pi`) are registered with their
      required secret keys and launch commands.
- [ ] Each provider's key is stored/injected via the OpenBao path from Phase 5.
- [ ] User can launch each of the four agents in their machine from the UI.
- [ ] Adding a hypothetical 5th provider requires only a registry entry + template,
      no control-plane code change (verified by design/review).

---

## Phase 7: GitHub git operations

**User stories**: As a user, I can clone one of my GitHub repos into my machine and commit
and push changes.

### What to build

Use the GitHub OAuth grant (stored in OpenBao) as the git credential inside the VM via a
git credential helper that fetches a short-lived token through the gateway/guest agent.
A repo picker in the UI lists the user's repos; clone pulls into the workspace; commit/push
work with the user's identity.

### Acceptance criteria

- [ ] `GET /api/git/repos` lists the user's GitHub repos.
- [ ] `POST /api/git/clone` clones a selected repo into the machine workspace using the
      OpenBao-backed token (no token persisted on disk in plaintext).
- [ ] git commit uses the user's GitHub identity (name/email).
- [ ] git push to an authorized repo succeeds from the machine.
- [ ] Token refresh/expiry is handled; a revoked GitHub grant cleanly fails git ops.

---

## Phase 8: code-server (robust editor / file browser)

**User stories**: As a user, I can browse and edit my project in a full VS Code in the
browser, replacing the basic file browser.

### What to build

Run code-server inside the microVM and proxy it through the gateway (`/gw/code-server/*`),
reusing the auth/ownership model from Phase 3. The React desktop embeds it as a window.
The PoC's simple file-read endpoints are retired.

### Acceptance criteria

- [ ] code-server runs in the VM and is reachable **only** via the authenticated gateway.
- [ ] User can open, edit, and save files in their workspace through code-server.
- [ ] Edits persist on the Phase 4 disk and are visible to terminal/agents and vice versa.
- [ ] code-server is scoped to the user's workspace; no host/other-tenant access.
- [ ] Old PoC file-browser endpoints are removed.

---

## Phase 9: React desktop UX

**User stories**: As a user, I get a full desktop experience — multiple windows for
terminals, the editor, agents, logs, and settings — on the new architecture.

### What to build

Rebuild the windowing desktop from the PoC (terminals, editor, logs, settings, live
machine status) as first-class React on top of the gateway and the new APIs. Multiple
concurrent terminal/agent windows, machine start/stop controls, live state and event log,
wallpaper/theme/settings.

### Acceptance criteria

- [ ] Multiple windows (terminals, code-server, agents, logs, settings) open concurrently.
- [ ] Machine start/stop/status and the `machine_events` stream are surfaced in the UI.
- [ ] Provider keys and GitHub connection are managed from a settings UI.
- [ ] Window state (layout) persists via the machine SQLite where appropriate.
- [ ] PoC `public/` desktop is fully replaced.

---

## Phase 10: Hardening & multi-tenancy

**User stories**: As an operator, I can be confident tenants are isolated from each other
and bounded in resource use.

### What to build

The security and tenancy controls that make multi-user safe: network isolation between
tenants, per-VM CPU/memory/disk quotas and limits, OpenBao per-user policy enforcement
end-to-end, audit logging in Postgres, gateway rate limiting, and input/authorization
hardening across all `/gw/*` and `/api/*` routes.

### Acceptance criteria

- [ ] A tenant's VM cannot reach another tenant's VM or the control-plane internals
      (verified by a network-isolation test).
- [ ] Per-VM CPU/memory/disk limits are enforced; a runaway workload can't starve the host.
- [ ] OpenBao policies provably prevent cross-user secret access.
- [ ] All security-relevant actions (auth, machine lifecycle, secret writes, git ops) are
      audited in Postgres.
- [ ] Gateway and auth routes are rate-limited; ownership checks cover every `/gw/*` path.

---

## Phase 11: Observability, scheduling & scale

**User stories**: As an operator, machines schedule across multiple hosts, idle machines
hibernate automatically, and the system self-heals from drift.

### What to build

Multi-host scheduling/placement (the `hosts` inventory becomes real), idle detection that
auto-hibernates machines, and full observability (metrics, structured logs, tracing). A
**reconciliation loop** continuously aligns Postgres state with actual VM state — directly
fixing the PoC's in-memory drift where the server forgot about running containers.

### Acceptance criteria

- [ ] The scheduler places machines across ≥2 hosts based on capacity.
- [ ] Idle machines auto-hibernate after a configurable threshold; resume on access.
- [ ] Metrics/logs/traces are emitted for control plane, node-agent, and gateway.
- [ ] A reconciliation loop detects and corrects DB↔VM drift (orphaned VMs, stale
      `running` records) without manual intervention.
- [ ] Killing a node-agent and restarting it recovers/repairs its machines' states.

---

## Phase 12: Production readiness

**User stories**: As an operator, I can deploy the whole system from scratch via a
pipeline and restore a user's machine from backup.

### What to build

The operational finish: infrastructure-as-code for all components, automated DB
migrations in the deploy path, base-image (kernel/rootfs) build pipeline, scheduled disk
backups with tested restore, and a CI/CD rollout with health-gated deploys.

### Acceptance criteria

- [ ] Full environment (control plane, node-agents, Postgres, OpenBao, networking) is
      provisioned from IaC.
- [ ] DB migrations run automatically and safely on deploy.
- [ ] microVM base images are built and versioned by a pipeline; machines can be pinned
      to / upgraded between image versions.
- [ ] Persistent disks are backed up on a schedule; a machine disk can be restored from
      backup (tested end-to-end).
- [ ] CI/CD deploys with health checks and a rollback path.

---

## Sequencing notes

- **Phases 1–4 are the spine** and are the thickest because they stand up an entirely new
  stack (Go, Firecracker, React) and the durable contracts. Each is still independently
  demoable. Resist adding features here.
- **Phases 5–9 are feature slices** on the stable spine and can be parallelized somewhat
  once the gateway (Phase 3) exists.
- **Phases 10–12 are the hardening tail** — necessary for production but deliberately
  deferred so value ships early. Some Phase 10 concerns (ownership checks, basic limits)
  should be applied opportunistically in earlier phases rather than left entirely to the end.
- **Risk hotspots**: Firecracker disk persistence + snapshot/resume (Phase 4) and the
  authenticated gateway proxy model (Phase 3) carry the most technical uncertainty —
  prototype these first within their phases.
