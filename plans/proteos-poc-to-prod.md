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
  **Trust model: the guest agent is untrusted.** The user has (or can gain) root inside
  their own VM, so anything the guest agent can request from the control plane carries
  only that user's authority. It authenticates with the per-machine identity from
  `secret/machines/<machine_id>/identity`, and the control plane must scope every request
  it honors to the owning user — never give the guest channel broader power than the user
  themselves has.

### Networking / reaching the VM

- Each microVM gets a **tap device + private IP** on a per-tenant/host bridge. No VM
  port is ever exposed publicly (this is the key departure from the PoC's direct
  `iframe → :7681`).
- All browser↔VM traffic (terminal WS, code-server HTTP/WS, agent I/O) is brokered by
  the **control-plane gateway**, which authorizes the session, looks up the user's VM,
  and proxies to the guest agent's private address.
- The gateway↔guest-agent channel is **authenticated, not just private**: either vsock
  (a host-side unix socket per VM — no IP layer to spoof at all) or tap + mTLS using the
  per-machine identity. "Private IP on a bridge" alone is not an auth boundary on a
  shared host.
- **Egress policy** — the VMs run untrusted, AI-generated code: default-deny from VMs to
  the control plane, OpenBao, host services, other hosts, and link-local/metadata
  addresses; NAT to the internet with per-tenant rate limits. Egress control is also the
  primary abuse control (cryptomining, scanning, spam originating from your IPs).

### VM / host security baseline (applies from Phase 2, not Phase 10)

- Every Firecracker VMM runs under **jailer**: chroot, dedicated uid/gid per VM, seccomp,
  and a cgroup enforcing CPU/memory/io on the VMM process. Retrofitting this later means
  re-testing every lifecycle path — it ships with the first VM.
- **Snapshots contain guest RAM**, including any secrets injected at start. Snapshot
  files and persistent disks are encrypted at rest and handled as secret material.
- **After resume**: reseed guest entropy (virtio-rng or a guest-agent reseed) and resync
  the guest clock — a restored VM otherwise resumes with duplicated RNG state and a stale
  clock, breaking TLS, git, and token-expiry checks. In-flight TCP/TLS connections die on
  resume; the guest agent must tolerate that.
- Base kernel/rootfs are **version-pinned from the first boot** (Phase 2). What Phase 12
  defers is the build *automation*, not reproducibility.

### Web origin isolation

Anything that can serve user-controlled HTML — code-server, app preview ports, agent web
UIs — must **not** share an origin with the SPA/API, or malicious workspace content can
script against the control-plane session (XSS → account takeover). Decide this in
Phase 3, because it shapes the gateway and DNS/TLS:

- Serve per-machine content from `m-<machine-id>.<domain>` via wildcard DNS + wildcard
  TLS. (This supersedes the path-shaped `/gw/code-server/*` route below where
  user-controlled HTML is involved — the gateway still proxies, on a separate hostname.)
- Auth on those subdomains uses a short-lived signed token minted by the main origin
  (cookie scoped to that subdomain) — never the main session cookie.
- All `/gw/*` WebSocket upgrades validate the `Origin` header (cross-site WebSocket
  hijacking is the WS equivalent of CSRF), and revoking a session terminates its live
  WS connections.

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

- **GitHub** (web flow) is primary identity. Prefer a **GitHub App** over a classic
  OAuth app: the classic `repo` scope is all-or-nothing across every repo the user can
  reach, while a GitHub App gives per-repo installation grants and short-lived (~1h),
  refreshable user tokens — exactly what the Phase 7 credential broker wants. The login
  flow looks the same to the user. Decide in Phase 1: re-onboarding users to a different
  grant type later is painful.
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
  proxy for the gateway; Firecracker via `firecracker-containerd` or the Firecracker SDK;
  Testcontainers for testing. No mocks.
- **Infra**: Postgres, OpenBao, compute hosts with KVM for Firecracker.

### Development environment (Firecracker requires Linux + KVM)

Firecracker only runs on Linux with `/dev/kvm` — there is no macOS support, so the real
VMM path can never run on a dev Mac. The strategy:

- **Hide the VMM behind a driver interface in the node-agent** (create/boot, stop,
  snapshot, resume, attach-disk, network setup). Ship two implementations: the real
  Firecracker driver, and a **dev driver** that fakes a "machine" with a local process or
  container. The control plane, gateway, React app, and even the guest agent (it's just a
  Linux binary) are then all developable on a Mac against the dev driver.
- **Real-Firecracker work happens on Linux dev VMs in the Proxmox cluster.** Enable
  nested virtualization on those VMs (CPU type `host`) so `/dev/kvm` exists inside them —
  Firecracker runs fine under nested KVM and performance is plenty for dev. Per-dev or
  shared VMs, driven over SSH / remote editing.
- **Integration tests that touch Firecracker need a KVM-capable CI runner** — a
  self-hosted runner on the Proxmox cluster, added to the Phase 1 CI when Phase 2 starts.
  Everything else tests against the driver interface and runs on any runner.
- **Match production architecture early**: if the Proxmox cluster (and eventual prod
  hosts) are x86_64, build kernels/rootfs for x86_64 from day one rather than fighting
  cross-arch on Apple Silicon.

### Open decisions (resolve by the phase noted)

- **Disk locality — DECIDED (2026-06-10): host-local disks.** Simple and fast; machines
  are pinned to their host, and host loss means restore-from-backup (Phase 12).
  Consequences to honor: Phase 11 scheduling places *new* machines only (no migration of
  existing ones), and backups are the only DR story. Keep disk attach/provision behind an
  interface in the node-agent so network block storage can be added later without
  touching the control plane; revisit if live migration or fast rescheduling becomes a
  requirement.
- **Firecracker integration** (Phase 2): `firecracker-containerd` vs driving the
  Firecracker API/SDK directly from the node-agent. The Task 2.0 spike runs against the
  raw API; decide after it, informed by its findings — snapshot/restore and disk-attach
  are where the two options differ most.
- **Gateway→guest transport** (Phase 3): vsock vs tap+mTLS (see Networking).
- **Cross-host gateway routing** (by Phase 11): any gateway instance must reach any VM on
  any host — routable per-host private subnets or an overlay (e.g. WireGuard mesh).

### Cross-cutting non-goals for early phases (added in the hardening tail)

Multi-host scheduling, auto-hibernate on idle, quotas, full audit/observability, backups,
and CD/deploy automation are deliberately deferred to Phases 10–12 so the spine ships
first. Two things are **not** deferred: **CI** (build + tests on every PR) starts in
Phase 1 — it's cheap and prevents the hardening tail from becoming a rewrite — and any
publicly reachable deployment stays behind a **signup allowlist** until Phase 10's abuse
controls exist, because free compute attracts cryptominers immediately.

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

- [x] `users` and `sessions` tables exist via migrations; Postgres is reachable from Go.
      (`controlplane/migrations/000001_init.*.sql`; `store.NewPool` + sqlc queries; store tests.)
- [x] GitHub OAuth login + callback works; a session cookie is issued; `GET /api/me`
      returns the authenticated user. (`internal/auth`; end-to-end test against a fake GitHub
      in `auth_test.go` — `TestHappyPathLogin`.)
- [x] Logout clears the session. (`Handler.Logout` revokes server-side + clears cookie;
      `TestLogoutRevokesSession`.)
- [x] React app shows a login screen when unauthenticated and a dashboard (with
      "no machine yet" empty state) when authenticated. (`web/src/routes/Login.tsx`,
      `Dashboard.tsx`; `RequireAuth` gate in `App.tsx`.)
- [x] GitHub tokens are written to a clearly-stubbed secrets interface (`secrets.Store` +
      dev `FileStore`; OpenBao lands in Phase 5) — **not** to Postgres. (`github_links` holds
      only `secret_ref`; invariant asserted in `TestHappyPathLogin`.)
- [x] Unauthenticated access to `/api/machine*` is rejected. (`requireAuth` on the prefix;
      table-driven `TestProtectedRoutesRejectUnauthenticated`.)
- [x] OAuth `state` is validated on the callback (HMAC-signed, 10-min expiry, constant-time);
      session cookie is httpOnly, Secure, SameSite=Lax. (`internal/auth/state.go`;
      `state_internal_test.go`.)
- [x] CI runs build + tests + migrations on every PR, from this phase onward.
      (`.github/workflows/ci.yml`: go vet/build/test -race with a Postgres service container,
      `sqlc diff`, migrations applied; web typecheck + build + artifact.)

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

### Task 2.0 — scripted Firecracker spike on a Proxmox VM (do this first)

A throwaway-in-code but **reproducible-in-process** run-through of every Firecracker
capability the node-agent driver will need. Lives in `spike/firecracker/` as numbered
scripts plus a README; manual steps are fine as long as they are script-driven and
documented. The scripts are not production code — their output is *findings* that feed
the driver design and the firecracker-containerd-vs-raw-API decision.

```
spike/firecracker/     (scaffolded 2026-06-10 — scripts exist, ready to run)
  README.md            run order, acceptance, findings template, gotchas log
  env.sh               pinned versions (firecracker v1.16.0) + all shared config
  lib.sh               helpers: API calls, boot/teardown, tap+NAT, guest SSH
  versions.lock        exact resolved artifacts, written by 01 — commit it
  00-proxmox-vm.md     manual doc: create the Proxmox VM (CPU type `host`, Ubuntu LTS,
                       cores/RAM/disk), verify nested KVM (/dev/kvm present, kvm-ok)
  01-host-setup.sh     install pinned firecracker + jailer binaries; fetch pinned
                       kernel + rootfs; verify /dev/kvm access as non-root
  02-boot-vm.sh        boot a microVM via the Firecracker API socket; reach a shell
                       on the serial console
  03-network.sh        tap + bridge + NAT: guest can curl the internet; host can reach
                       the guest's private IP (and nothing else can)
  04-attach-disk.sh    create an ext4 image, attach as a second drive, mount in guest,
                       write a file, verify it survives a full stop + cold boot
  05-snapshot-restore.sh  pause → snapshot (memory + state) → kill the VMM → restore in
                       a fresh VMM process; verify the file and a running process
                       survive; observe and record clock skew and entropy state
  06-jailer.sh         repeat the boot under jailer (chroot, per-VM uid, cgroups) —
                       proves the security baseline works before the driver is written
  07-teardown.sh       remove taps, processes, images; leave the VM clean for a rerun
```

**Spike acceptance criteria:**

- [x] A second engineer reproduces the entire run on a fresh Proxmox VM using only the
      README and scripts — no tribal knowledge.
- [x] Every driver-interface capability is demonstrated: boot, network, disk
      attach/persist, snapshot/restore, jailer.
- [x] Clock-skew and entropy behavior after restore are observed and written down (these
      feed the Phase 4 resume criteria).
- [ ] Findings (pinned versions, timings, surprises) are recorded in the README and feed
      the driver design and the firecracker-containerd vs raw-API decision.

### Acceptance criteria

> **Verification status (2026-06-11).** Full stack run on the Proxmox VM per
> `RUNBOOK.md`. Items marked *(live)* were demonstrated end-to-end on that run;
> *(suite)* were verified by the Phase 2 automated tests but not re-clicked on the
> live stack today — worth a manual pass to fully close out (see note after the list).

- [x] `machines`, `machine_events`, `hosts` (minimal) tables exist. *(live)* —
      migration `000002`, applied on the Proxmox stack via control-plane `-migrate`.
- [x] `POST /api/machine` creates a record (`provisioning`) and the node-agent boots a
      real Firecracker microVM that reaches `running`. *(live)* — create →
      provisioning → running on the Proxmox VM; guest reachable on its private IP.
- [x] `POST /api/machine/stop` and `/start` transition the VM and persist state.
      *(suite)* — `machine.Service` + poller; lifecycle e2e in controlplane tests
      and DevDriver; FC `Stop`/`SendCtrlAltDel`→kill + cold-boot `start` in the driver.
- [x] Each VM gets a tap device + private IP; the control plane records `vm_handle`/host.
      *(live)* — tap `tapNNNNNNNN`, `guest_ip` (172.30.0.2) and host recorded on the row.
- [x] Every transition writes a `machine_events` row. *(suite)* — guarded CAS +
      event insert in one pgx tx (`machine.Transition`); asserted on every path in 2.4.
- [x] Dashboard reflects live machine state; `GET /api/machine/events` streams updates.
      *(live login + create; suite for replay)* — SSE broker + EventSource;
      `Last-Event-ID` replay test in 2.5.
- [x] Node-agent ↔ control-plane channel is authenticated (not open on the network).
      *(live)* — shared bearer token (constant-time compare), dialed across the LAN.
- [x] Every VMM runs under jailer (chroot, per-VM uid, seccomp, cgroup limits) from the
      first boot. *(suite + spike)* — driver execs jailer (`--chroot-base-dir`,
      per-VM uid, cgroup v2); proven in spike `06-jailer.sh` and the FC integration test.
- [x] Base kernel/rootfs version is pinned and recorded on the machine record. *(live)* —
      `kernel_ref`/`rootfs_ref` stamped per machine; images pinned via spike `versions.lock`.
- [x] VMs cannot reach the control plane, node-agent, or host services (basic
      default-deny egress; the full per-tenant policy lands in Phase 10). *(live)* —
      guest→gateway:9090 and guest→host blocked (nft `input`-hook deny), guest→RFC1918
      dropped (forward deny), guest→internet works via NAT masquerade. Note: a
      default-deny system `FORWARD` policy (Docker/ufw) required the driver to add tap
      accepts into the `ip filter` FORWARD chain — see `RUNBOOK.md` gotchas.
- [x] The node-agent's VMM access sits behind a driver interface with a non-KVM dev
      driver, so the full stack runs on a dev Mac; the Firecracker driver runs on the
      Proxmox Linux VMs (nested KVM) and in a KVM-capable CI job. *(live + gated)* —
      `Driver` interface + DevDriver (Mac); FC driver verified live on Proxmox; KVM CI
      job present but gated on `vars.HAS_KVM_RUNNER` until the self-hosted runner exists.

> **To fully close out the *(suite)* items on the live stack:** click Stop then Start
> on the dashboard and confirm the state badge + `machine_events` rows
> (`requested→provisioning→running→stopping→stopped→starting→running`); watch a second
> browser tab update live over SSE; and `ps -o uid,cmd -C firecracker` on the host to
> confirm the VMM runs under the per-VM jail uid (≥100000), not root.

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

- [x] Guest agent runs inside the microVM and exposes a PTY over WS on the private network.
- [x] `WS /gw/terminal` authenticates the user, authorizes ownership of the target VM,
      and proxies to the guest — **no VM port is publicly reachable**.
- [x] React terminal is interactive (input/output, resize) against a real shell in the VM.
- [x] Terminal access is denied for users who don't own the machine / aren't logged in.
- [x] PTY sessions live in the guest agent, decoupled from the WS connection: a dropped
      WS reconnects to the same shell with scrollback intact (tmux-like semantics).
- [x] Gateway↔guest channel is authenticated (vsock or mTLS); WS upgrades validate
      `Origin`; logging out / revoking a session closes its live connections.

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

- [x] A persistent disk is provisioned per machine and attached at boot; survives
      stop/start and host process restarts.
- [x] `stop` snapshots/hibernates the VM; `start` resumes (or cold-boots) with the disk
      reattached and state intact.
- [x] Machine SQLite is initialized on the disk and used by the guest agent.
- [x] A file written in the terminal persists across a stop/start cycle (demoable).
- [x] `disk_id` and snapshot metadata are recorded in Postgres.
- [ ] Disk and snapshot files are encrypted at rest; snapshots are handled as secret
      material (they contain guest RAM).
- [ ] Resume reseeds guest entropy and resyncs the guest clock (TLS, git, and token
      expiry break otherwise).

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
- [ ] Secrets are injected on every start **and resume** (not only first boot), and
      secret reads/writes are audited (an early slice of Phase 10's audit log).

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
- [ ] Tokens handed to the VM are short-lived (GitHub App installation/user tokens, per
      the Auth decision) and fetched on demand by the credential helper — never written
      to disk inside the VM.

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
- [ ] code-server (and any preview port) is served from a per-machine subdomain,
      origin-isolated from the SPA/API per the "Web origin isolation" decision —
      workspace content can never script against the control-plane session.
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
tenants, the full **per-tenant egress policy** (internal default-deny, internet rate
limits — see the security baseline), per-VM CPU/memory/disk quotas and limits, OpenBao
per-user policy enforcement end-to-end, audit logging in Postgres, gateway rate limiting,
abuse controls (signup gating, egress/compute quotas), and input/authorization hardening
across all `/gw/*` and `/api/*` routes.

### Acceptance criteria

- [ ] A tenant's VM cannot reach another tenant's VM or the control-plane internals
      (verified by a network-isolation test).
- [ ] Per-VM CPU/memory/disk limits are enforced; a runaway workload can't starve the host.
- [ ] OpenBao policies provably prevent cross-user secret access.
- [ ] All security-relevant actions (auth, machine lifecycle, secret writes, git ops) are
      audited in Postgres.
- [ ] Gateway and auth routes are rate-limited; ownership checks cover every `/gw/*` path.
- [ ] VM egress to internal targets is default-denied and internet egress is rate-limited
      per tenant (verified by tests run from inside a VM).

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
      "Idle" must account for agent activity reported by the guest agent — a machine
      running a long AI-agent task is not idle, even with no terminal input.
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
- [ ] Postgres and OpenBao are backed up and restorable; OpenBao runs HA with an
      auto-unseal strategy (a vault that only unseals by hand is an outage multiplier).
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
  prototype these first within their phases. The scripted Firecracker spike (Task 2.0)
  burns down most of the Firecracker unknowns before any production code exists.
- **Decide early even if built late**: web origin isolation (Phase 3 — it shapes gateway
  auth and DNS/TLS), disk locality (Phase 2 design — it constrains Phase 11 scheduling
  and Phase 12 DR), and GitHub App vs OAuth app (Phase 1 — re-onboarding users to a
  different grant type later is painful). See "Open decisions".
- **Until Phase 12 backups exist, a dead host loses its machines' disks** (disks are
  host-local — see Open decisions). Acceptable for a gated beta, but state it to early
  users; or pull basic disk snapshots forward into Phase 4 if that risk is unacceptable.
