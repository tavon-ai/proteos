# Phase 3 Implementation Plan: Browser terminal through the gateway

> Source: `plans/proteos-poc-to-prod.md` Phase 3, planned 2026-06-10. Status: **not started**.
> Prerequisite: Phase 2 (`plans/phase-2-implementation.md`) — also not yet implemented. Phase 3 treats its contracts (nodeagent module, `agentapi`, bearer token, driver interface, machines/hosts tables, machine.Service/broker) as given. Tasks note which parts can start before Phase 2 lands (3.0, 3.1, 3.2 are fully buildable today).

## Context

Phase 1 shipped auth/sessions/SPA; Phase 2 (planned) ships machines that reach `running` behind a node-agent driver. Phase 3 makes a machine *usable*: a new **guest agent** runs inside the VM and owns PTY sessions (tmux-like: sessions survive WS drops, scrollback replays on reattach); the control plane gains the **gateway** (`WS /gw/terminal`) that authenticates the user, authorizes machine ownership, and relays the terminal protocol to the guest; the React app renders it with xterm.js. This proves the browser→gateway→guest path that code-server (Phase 8) and AI agents (Phases 5–6) reuse.

**Decisions locked (2026-06-10):**
- **Transport: vsock.** Firecracker virtio-vsock; host-side unix socket per VM inside the jail chroot; the **node-agent bridges gateway→guest** over its existing bearer-authenticated channel. Pre-answers Phase 11 cross-host routing. The spike gains an `08-vsock.sh` script to validate this (none exists today).
- **Web origin isolation: committed now, built in Phase 8.** Per-machine content will live on `m-<machine-id>.<domain>` (wildcard DNS/TLS, short-lived subdomain tokens). The Phase 3 terminal WS is not user-controlled HTML, so it stays on the main origin at `/gw/terminal`; the gateway keeps auth / target-resolution / proxy as separable steps so the subdomain path slots in without rework.

## Architecture decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | **Separate Go module `guestagent/`** added to the root `go.work`. Wire protocol types in `guestagent/api` (package `guestwire`), imported by controlplane (same pattern as `agentapi`); guestagent imports nothing from other modules. | Different deploy story (static Linux binary baked into the rootfs); keeps `creack/pty`/`mdlayher/vsock` out of other go.sums. |
| 2 | **Guest agent ships baked into a custom rootfs**: `image/build-rootfs.sh` (Linux-only, sudo loop-mount) copies the pinned Firecracker-CI base ext4, installs a static `guestagent` binary (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64`, version via `-ldflags -X`), a systemd unit `proteos-guestagent.service`, `/etc/proteos-release`; output `proteos-rootfs-<base>-ga<gitshort>.ext4` + sha256 in `image/manifest.lock`. `PROTEOS_ROOTFS_REF` points at it; the per-machine `rootfs_ref` (Phase 2) pins it. (Note: existing `images/` dir holds README screenshots — new dir is `image/`.) | Simplest reproducible option: one offline build step, no per-boot injection machinery, snapshot-friendly (Phase 4 restores RAM — an init-time injector would be dead code there). Deliberate seed of Phase 12's image pipeline. |
| 3 | **vsock device per VM, fixed guest port 1024, `guest_cid: 3` for every VM.** `PUT /vsock` pre-boot (no hot-add, like NICs) with `uds_path` inside the jail chroot; host-initiated connects use Firecracker's hybrid handshake (`CONNECT 1024\n` → `OK <port>\n`). CID 3 everywhere is fine because the host never uses AF_VSOCK — each VM has its own uds. | The master plan's named no-spoof option. Fixed port + fixed CID = zero allocation state. Validated by spike `08-vsock.sh` first (incl. jailer + snapshot/restore behavior, recorded for Phase 4 — host uds re-creation on restore is a known sharp edge). |
| 4 | **Node-agent stays a dumb byte pipe**: new route `GET /v1/machines/{id}/guest` with `Upgrade: proteos-guest` (bearer-authenticated). Handler checks machine running, calls new driver method `DialGuest(ctx, machineID) (net.Conn, error)` (Firecracker: jail uds + CONNECT handshake; Dev: unix socket), replies 101, hijacks, copies both ways. | One protocol end-to-end (gateway↔guest speak WS; node-agent never parses frames) — terminal, code-server, and agent traffic all reuse this exact tunnel. HTTP Upgrade beats CONNECT (awkward in ServeMux) and beats WS re-termination in the node-agent (double framing, more trusted code). |
| 5 | **DevDriver launches the real guest-agent binary** per machine when `PROTEOS_DEV_GUESTAGENT_BIN` is set (else the Phase 2 stub), with `PROTEOS_GUEST_LISTEN=unix:<datadir>/machines/<id>/guest.sock` (0700 dir). `DialGuest` = unix dial. | The whole browser→gateway→node-agent→guest path runs on a Mac with zero fakes except the hypervisor, through the identical bridge code path. |
| 6 | **Terminal protocol: one WS; binary frames = raw PTY bytes; text frames = small JSON control messages** (`hello`, `resize`, `exit`). Session chosen by `?session=` at upgrade (default `main`). On attach: `hello`, then scrollback ring (last 256 KiB) as binary, then live output. Shell `/bin/bash -l` as root in the guest (`TERM=xterm-256color`); session persists across WS drops; concurrent attaches fan out output / merge input; shell exit ⇒ `exit` frame + close; next attach spawns fresh. Keepalive = WS-native pings every 30s. | Binary-for-data avoids base64 and maps 1:1 to xterm.js ArrayBuffer handling; query-param attach keeps the gateway relaying messages opaquely. Multiple sessions / non-root user / idle reaping are Phase 9+. |
| 7 | **Gateway terminates the browser WS, dials a guest WS through the tunnel, relays messages 1:1** (type-preserving loops, ctx-cancelled). Library: **coder/websocket** everywhere — context-first, maintained, and its client dials through Go's protocol-switch (one-shot `http.Client` whose `DialContext` returns the hijacked tunnel conn, keep-alives off). | Relaying messages (not raw byte splice) lets the gateway own auth/Origin/close-codes; the guest agent stays a plain httptest-testable WS server. gorilla rejected: callback-style, no context plumbing. |
| 8 | **`GET /gw/terminal` = requireAuth + explicit Origin middleware + ownership resolution as separable steps.** Origin must be present and exactly equal one of `PROTEOS_ALLOWED_WS_ORIGINS` (default = `PROTEOS_BASE_URL` origin; dev adds `http://localhost:5173`). Browser WS can't send `X-Requested-By` — the Origin check is the WS-equivalent of CSRF. Handler: `resolveMachine(user, ?machine=)` (foreign id → 404, avoids existence leak) → must be `running` else 409 → dial. Mid-session tunnel EOF → re-fetch state → close `4002 machine_stopped` (else 1011). | Separable resolve/authorize/dial is exactly what Phase 8 swaps for subdomain-token auth + code-server targets without touching the proxy core. |
| 9 | **Session revocation closes live WS via an in-process registry** keyed by session id: `session.Manager.Authenticate` extended to return the session row (middleware puts session id in context); `Revoke` uses `RevokeSession … RETURNING id` and notifies a `RevocationListener` — the gateway registry closes that session's conns with `4001 session_revoked`. | Exact and immediate; proportionate to a single control-plane instance. Periodic per-conn revalidation deferred to Phase 10/11 (multi-instance needs it anyway). |
| 10 | **No app-layer credential gateway→guest this phase.** Chain: gateway→node-agent = Phase 2 bearer token; node-agent→guest = per-VM jailed uds reachable only by host root — vsock is the plan's named "no IP layer to spoof" option, so "authenticated, not just private" holds by construction. Per-machine identity (`secret/machines/<id>/identity`, OpenBao) adds app-layer auth in Phase 5; documented in guestagent README + a comment at the listener. | No secret store exists until Phase 5; a Phase 3-only credential adds rotation/injection machinery Phase 5 replaces. The trust boundary is real, not deferred. |
| 11 | **React terminal: `@xterm/xterm` + `@xterm/addon-fit`** in a Dashboard panel/modal opened from MachineCard when `running` (windowing is Phase 9). `lib/terminalSocket.ts`: `binaryType="arraybuffer"`; input → binary frames; output `Uint8Array` → `term.write`; ResizeObserver → fit → debounced (100ms) resize frame; reconnect with backoff (0.5s→8s cap) while panel open; `term.reset()` before each replay so scrollback isn't duplicated; status banner; no retry on 401/403/404/409 upgrade failures. Vite proxy adds `/gw` → `:8080` with `ws: true`. | Current scoped xterm packages; reset-before-replay makes ring-buffer replay idempotent in the UI. |

## Wire contracts

### Firecracker vsock device (FirecrackerDriver, pre-boot)

```
PUT /vsock  {"guest_cid": 3, "uds_path": "/v.sock"}      # path relative to jail root
# host file: <chroot-base>/firecracker/<id>/root/v.sock (created by Firecracker, jailer uid)
# host-initiated connect (in DialGuest):
unix-connect(.../root/v.sock) → send "CONNECT 1024\n" → expect "OK <port>\n" → raw bytes
```

### Node-agent guest tunnel (bearer `Authorization: Bearer $PROTEOS_AGENT_TOKEN`)

```
GET /v1/machines/{id}/guest        (Connection: Upgrade, Upgrade: proteos-guest)
→ 101 Switching Protocols, then an opaque bidirectional byte stream bridged to the
  VM's vsock port 1024 (dev: the machine's guest.sock)
→ 404 unknown_machine | 409 not_running | 502 guest_unreachable
```

### Terminal WS protocol (browser ↔ gateway ↔ guest; types in `guestagent/api`, pkg `guestwire`)

```
guest agent: GET /terminal?session=main                  (session name [a-z0-9-]{1,32}, default "main")
gateway:     GET /gw/terminal?session=main[&machine=<uuid>]   (cookie auth + Origin check)

Binary frames: client→guest raw input; guest→client raw PTY output
               (on attach: scrollback replay ≤256 KiB before live output)
Text frames (JSON):
  guest→client {"type":"hello","session":"main","replay_bytes":N}
  client→guest {"type":"resize","cols":120,"rows":32}
  guest→client {"type":"exit","exit_code":0}             # then WS close 1000

Close codes (gateway→browser): 1000 normal · 4001 session_revoked · 4002 machine_stopped · 1011 internal
Pre-upgrade HTTP errors: 401 unauthorized · 403 origin_forbidden · 404 no_machine · 409 machine_not_running
Keepalive: WS ping frames every 30s on both legs.
```

### Config additions

```
controlplane: PROTEOS_ALLOWED_WS_ORIGINS  (CSV, default = PROTEOS_BASE_URL origin)
nodeagent:    PROTEOS_GUEST_VSOCK_PORT=1024, PROTEOS_DEV_GUESTAGENT_BIN=<path>
guestagent:   PROTEOS_GUEST_LISTEN=vsock:1024|unix:/path|tcp:127.0.0.1:7070,
              PROTEOS_GUEST_SHELL=/bin/bash, PROTEOS_GUEST_SCROLLBACK_KIB=256
```

## Package layout

```
go.work                                   # + ./guestagent
guestagent/
  go.mod                                  # module github.com/tavon/proteos/guestagent
  cmd/guestagent/main.go
  api/                                    # package guestwire — frame types, close codes, session-name rules
  internal/
    config/
    term/                                 # Session (creack/pty + ring buffer + attach fan-out), Manager
    server/                               # WS handler /terminal, attach loop, ping loop
    listen/                               # Listen(spec): vsock_linux.go (mdlayher/vsock), unix, tcp
nodeagent/internal/
  driver/                                 # + GuestDialer: DialGuest(ctx, id) (net.Conn, error)
  driver/dev/                             # exec guestagent binary per machine; unix-socket dial
  driver/firecracker/                     # PUT /vsock pre-boot; uds CONNECT-handshake dial
  httpapi/                                # + GET /v1/machines/{id}/guest (hijack + copy)
controlplane/internal/
  gateway/                                # terminal.go, origin.go, registry.go (revocation), guestdial.go, relay.go
  nodeclient/                             # + DialGuest(ctx, machineID) (net.Conn, error)
  session/                                # Authenticate → (User, Session); Revoke RETURNING id + listener
web/src/
  lib/terminalSocket.ts
  components/Terminal.tsx, TerminalPanel.tsx    # + MachineCard "Open terminal" button
image/
  build-rootfs.sh  proteos-guestagent.service  manifest.lock  README.md
spike/firecracker/08-vsock.sh
```

## Tasks (Track A = Mac/dev-driver; Track B = Proxmox/Firecracker)

### 3.0 — Spike `08-vsock.sh` (Track B; standalone)
Boot with vsock device, guest listener on port 1024, host connects via uds CONNECT/OK handshake, echo verified; repeat under jailer (uds in chroot, ownership/perms); repeat across snapshot/restore — record whether the host uds survives or needs re-creation, and in-flight connection behavior (feeds Phase 4).
**Done when:** echo works plain + jailed; restore behavior written in spike README findings.

### 3.1 — guestagent module: PTY session core (Track A; standalone — needs nothing from Phase 2)
Module + go.work entry; `internal/term`: Session (spawn `$PROTEOS_GUEST_SHELL -l` on a creack/pty PTY; reader drains into 256 KiB ring always — attached or not; fan-out to attached writers; merged input; `Resize`; exit detection), Manager (named registry, create-on-first-attach, remove-on-exit). CI: guestagent job.
**Done when:** unit tests prove echo, detached-output capture, resize via `stty size`, exit detection — on macOS and Linux.

### 3.2 — guestagent: WS server + protocol + listeners (Track A; after 3.1)
`guestwire` types + close codes; server upgrade at `/terminal` (coder/websocket; origin check off — not browser-facing; comment pointing at decision #10), attach loop (hello → replay → live, resize/exit, 30s pings); `internal/listen` (`vsock:` behind linux build tag, `unix:`, `tcp:`); cmd wiring + version stamp.
**Done when:** WS test client over unix/TCP gets an interactive shell; drop + reattach replays scrollback including output emitted while detached; two concurrent attaches both receive output.

### 3.3 — node-agent guest tunnel (Track A; needs Phase 2 task 2.3)
Driver gains `DialGuest`; DevDriver execs `PROTEOS_DEV_GUESTAGENT_BIN` per machine (guest.sock in state, kill on Stop/Destroy, re-attach on agent restart); httpapi adds `GET /v1/machines/{id}/guest` (bearer, state check, `http.Hijacker`, bidirectional copy, teardown on either EOF).
**Done when:** a test dials the route with raw HTTP Upgrade, completes a WS handshake *through it* to a real guest agent, runs an echo; stopping the machine closes the tunnel.

### 3.4 — Control-plane gateway (Track A; needs Phase 2 tasks 2.4/2.5 + 3.3)
`session.Manager.Authenticate` returns `(User, Session)` (fix middleware/tests; session id into context); `RevokeSession … RETURNING id` + `RevocationListener`. `internal/gateway`: origin middleware, conn registry (close 4001 on revoke), `nodeclient.DialGuest` (manual Upgrade: dial, `req.Write`, `http.ReadResponse`, expect 101, return conn w/ buffered leftovers), guest WS dial through the tunnel, message relay with close-code mapping (tunnel EOF → state lookup → 4002/1011). Register `GET /gw/terminal` behind requireAuth + origin middleware; hook registry into Logout/Revoke; config plumbing.
**Done when:** e2e test (httptest control plane + real node-agent over DevDriver + real guest agent) round-trips a command; authz table: 401 unauthenticated, 403 bad/missing Origin, 404 no machine / foreign `?machine=`, 409 not running; logout mid-session closes WS with 4001.

### 3.5 — React terminal (Track A; after 3.4)
`@xterm/xterm` + `@xterm/addon-fit`; vite `/gw` ws proxy; `terminalSocket.ts` (arraybuffer, backoff reconnect, reset-before-replay, status callbacks, no-retry on 4xx); `Terminal.tsx` + `TerminalPanel` modal; MachineCard "Open terminal" enabled when `running`.
**Done when:** on a Mac dev stack the terminal is interactive; kill/restore network shows reconnecting then same shell with scrollback; stopping the machine shows "machine stopped"; resize reflows correctly.

### 3.6 — Rootfs bake + FirecrackerDriver vsock (Track B; after 3.0 + 3.2 + Phase 2 task 2.7)
`image/build-rootfs.sh` per decision #2 (assert the pinned base uses systemd in-script, fail loudly otherwise) + unit file + `manifest.lock`; FirecrackerDriver: `PUT /vsock` before `InstanceStart`, `DialGuest` uds handshake; update `PROTEOS_ROOTFS_REF` on the Proxmox VM.
**Done when:** a VM booted by the node-agent from the baked rootfs has the guest agent running at boot and `DialGuest` reaches it through the jailed uds.

### 3.7 — KVM integration test + acceptance pass (Track B; after 3.4 + 3.6)
`//go:build firecracker` test: boot baked rootfs → DialGuest → WS handshake → echo round-trip → drop → reattach → assert replay contains the marker; add to the gated KVM CI job. Run the full stack against the Proxmox node-agent; walk every Phase 3 acceptance criterion and check the boxes in `plans/proteos-poc-to-prod.md`; verify from outside the host that no guest port is reachable.

### Sequencing

```
3.0 (Track B spike) ────────────────────────────────┐
3.1 ──► 3.2 ──┬─────────────────────────► 3.6 ──────┤
              └► 3.3 ──► 3.4 ──► 3.5                ▼
                                            ──────► 3.7
Buildable before Phase 2 lands: 3.0, 3.1, 3.2. (3.3 needs P2 2.3; 3.4 needs P2 2.4/2.5; 3.6 needs P2 2.7.)
```

## Acceptance-criteria mapping (plan-doc Phase 3 checklist)

| Criterion | Task |
|---|---|
| Guest agent in the microVM, PTY over WS on private transport | 3.1 + 3.2 + 3.6 |
| `WS /gw/terminal` authn + ownership + proxy; no VM port public | 3.4 + 3.7 (negative check) |
| React terminal interactive (I/O, resize) against a real VM shell | 3.5 + 3.7 |
| Denied for non-owners / logged-out users | 3.4 (authz table) |
| Sessions decoupled from WS; reconnect with scrollback intact | 3.1 + 3.2 (mechanism), 3.5 (UX), 3.7 (real VM) |
| Authenticated gateway↔guest channel (vsock); Origin validation; revoke closes WS | 3.0 + 3.3 + 3.6 (vsock chain, decision #10); 3.4 (Origin + registry) |

## Critical existing-or-Phase-2 files to modify

- `controlplane/internal/httpapi/server.go` — register `GET /gw/terminal`; thread gateway deps
- `controlplane/internal/httpapi/middleware.go` — session id into context
- `controlplane/internal/session/session.go` + `internal/store/queries.sql` — Authenticate returns session; RevokeSession RETURNING id; revocation listener
- `controlplane/internal/config/config.go`, `cmd/controlplane/main.go` — WS origins, gateway wiring
- `controlplane/internal/nodeclient/` (Phase 2) — DialGuest tunnel dial
- `nodeagent/internal/driver/{driver.go,dev/,firecracker/}`, `nodeagent/internal/httpapi/` (Phase 2) — GuestDialer, guest-agent child, tunnel route
- `web/vite.config.ts`, `web/src/api/client.ts`, MachineCard (Phase 2), `Dashboard.tsx`
- `.github/workflows/ci.yml` — guestagent job; extend gated firecracker job
- `spike/firecracker/README.md` — 08 row + vsock findings; `go.work` — add `./guestagent`

## Verification

- **Unit/integration (any OS)**: `go test -race ./...` in all three modules — PTY echo/detached-output/replay/resize/exit; node-agent tunnel over DevDriver + real guestagent incl. stop-closes-tunnel; gateway e2e + authz table + revocation-closes-WS + bad-Origin.
- **Manual e2e (Mac)**: Postgres, node-agent (DevDriver + `PROTEOS_DEV_GUESTAGENT_BIN`), control plane, `npm run dev`; open terminal, type, resize, kill/restore network, stop machine mid-session, logout from a second tab and watch the terminal close.
- **Proxmox**: 3.0 spike green; 3.7 `-tags=firecracker` byte-path test; full checklist walkthrough; external port scan shows nothing guest-side reachable.
- **CI**: guestagent job green; cross-module `guestwire` import via go.work works in existing jobs; gated KVM job runs 3.7.

## Non-goals / deferred

- Multiple named sessions per machine, non-root shell user, idle reaping, window layout — Phase 9 (protocol's `session` param + registry already support names).
- App-layer gateway→guest credential (per-machine identity via OpenBao) — Phase 5 (decision #10 documents the vsock trust argument until then).
- Per-machine subdomains, wildcard DNS/TLS, subdomain tokens — Phase 8 (gateway steps kept separable).
- code-server / agent proxying — Phases 8/5–6 (reuse the 3.3 tunnel + 3.4 handler skeleton).
- Multi-instance revocation fan-out, periodic per-conn revalidation — Phase 10/11.
- vsock-across-snapshot handling — Phase 4 (3.0 records the findings it needs).
- Image build automation — Phase 12 (`image/build-rootfs.sh` is the pinned manual seed).
