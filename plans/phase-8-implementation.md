# Phase 8 Implementation Plan: code-server (robust editor / file browser)

> Source: `plans/proteos-poc-to-prod.md` Phase 8, planned 2026-06-11. Status: **not started.**
> Prerequisites: Phase 3 landed (tunnel, gateway, guest agent) — that is the hard
> dependency. Phase 4 (persistent disk) is planned but not landed: the editor works
> without it, but the "edits persist on the Phase 4 disk" criterion and durable
> settings/extensions only close after it lands (same gating note as Phases 5–7).
> Phases 5–7 are independent of this work except where noted (revocation registry and
> the Phase 7 control channel are touched but not required).
>
> This is the phase that **builds the web-origin-isolation decision** (master plan,
> "Web origin isolation"): per-machine subdomains, wildcard DNS/TLS, and subdomain-scoped
> auth. The gateway also grows its first **HTTP reverse-proxy** path — until now it only
> relays WebSocket messages.

## Context

The PoC's file browser (Express endpoints `server/index.js:206,251` feeding a vanilla-JS
window) is replaced by **code-server inside the microVM**, reached only through the
gateway. Three repo facts anchor the design:

1. **The session cookie is already host-only** (`internal/auth/auth.go:201` — no `Domain`
   attribute), so it is never sent to `m-<id>.<domain>` subdomains. The origin-isolation
   precondition holds today; Phase 8 adds the regression test that keeps it true.
2. **The guest tunnel is single-port**: `DialGuest` performs the vsock `CONNECT` handshake
   against one configured port (1024, the guest agent). code-server is a second,
   protocol-opaque HTTP service — it needs its own guest port and a port parameter
   through the tunnel chain (driver → agentapi → nodeclient), not a path carve-out on
   the guest agent's mux.
3. **The deploy path routes by path, not host**: nginx's catch-all (`server_name _`)
   serves the SPA for any Host, so subdomain requests would today get `index.html`. A
   wildcard server block (with the same Upgrade-header care that bit `/gw/` before — see
   `RUNBOOK.md` gotchas) plus wildcard DNS/TLS at the proxy layer (NPMplus) is part of
   the deliverable, not an afterthought.

One criterion needs honest interpretation: "code-server is scoped to the user's
workspace; no host/other-tenant access." Inside the VM, code-server runs with the same
authority the user already has in their terminal (root in their own microVM) — *VM
isolation is the boundary*, exactly as it is for the shell. "Scoped to the workspace"
means it opens `/workspace` by default and its reach ends at the VM wall; it does not
mean an in-VM ACL weaker than the terminal the user already owns.

## Architecture decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | **Subdomain scheme `m-<machine-uuid>.<PROTEOS_MACHINE_DOMAIN>`**, routed in the control plane by `Host` *before* the path mux: a machine-web host serves **only** `/__proteos/auth` and the proxy (never `/api`, never the SPA); the main host never serves machine-web paths. Dev/e2e use `m-<id>.localhost:<port>` (RFC 6761 loopback — no DNS or vite changes needed). The label scheme reserves `m-<uuid>-p<port>` for future preview ports (Phase 9+), unused now. | Host-first routing makes the origin split structural: code on a machine subdomain cannot reach API routes on that origin even if a proxy bug appears, and vice versa. Full UUID keeps labels valid (38 chars) and unguessable-ish without being load-bearing (authz is, see #3). |
| 2 | **Subdomain auth = short-lived mint + partitioned subdomain cookie.** `POST /api/machine/web-session` (main origin, requireAuth) mints an HMAC token (existing state-key pattern): claims `{machine_id, user_id, session_id, exp ≤ 60s}`. The SPA navigates the editor frame to `https://m-<id>.<dom>/__proteos/auth?token=…`; the gateway validates and sets the **subdomain-scoped** cookie `proteos_machine` — `HttpOnly; Secure; SameSite=None; Partitioned; Path=/` — whose value is itself signed and bound to the parent session id, then redirects to `/`. Every subsequent request on that host: validate cookie signature → parent session still alive → user still owns this machine → proxy. The main session cookie plays no part (and cannot arrive — fact #1). | Exactly the master-plan prescription ("short-lived signed token minted by the main origin… never the main session cookie"). Binding the subdomain cookie to the parent session makes logout/revocation effective on the editor with one check. `SameSite=None; Partitioned` (CHIPS) is what makes an embedded cross-origin iframe carry cookies in 2026 browsers — verified, with fallback, in 8.0/decision #6. |
| 3 | **Per-request authorization + revocation parity with Phase 3**: HTTP requests re-validate session+ownership on every request (cheap: session row already cached per the middleware; no new state); proxied **WebSocket upgrades** (code-server's own sockets) register in the Phase 3 revocation registry under the parent session id, so logout closes live editor sockets with the same `4001` semantics as terminals. WS upgrades on the subdomain validate `Origin == https://m-<id>.<dom>`. | "Reachable only via the authenticated gateway" must survive session revocation mid-edit, not just at connect time. Reusing the registry keeps one revocation story; Origin-on-subdomain closes CSWSH against code-server itself. |
| 4 | **Transport: a second guest vsock port (1025) raw-forwarded by the guest agent** to `127.0.0.1:13337` (code-server, loopback-only). `DialGuest` gains a `port` parameter end-to-end: driver interface, `GET /v1/machines/{id}/guest?port=`, nodeclient; DevDriver exposes a second unix socket per machine. The gateway's machine-web handler is a `httputil.ReverseProxy` whose transport dials the tunnel to port 1025 (Go's ReverseProxy passes Upgrade/WebSocket through natively); `X-Forwarded-Proto/Host` set, hop-by-hop stripped. | code-server speaks plain HTTP+WS and must stay path-untouched (no prefix rewriting — historically the fragile part of proxying it). A dedicated port keeps the guest agent's own mux (`/terminal`, `/control`, `/secrets`) out of the editor's namespace and gives later guest web services (previews) the same shape. Raw byte forward in the guest = no second HTTP parser in the trust path. |
| 5 | **code-server is baked pinned, supervised lazily by the guest agent**: standalone release install in `image/build-rootfs.sh` (version + sha256 in `manifest.lock`); guest agent starts it on the first web-port connection (`--auth none --bind-addr 127.0.0.1:13337 --disable-telemetry --disable-update-check`, default folder `/workspace`, user-data/extensions under `$HOME` → durable once Phase 4 lands), health-waits before forwarding, restarts on crash with backoff. `--auth none` is safe because 13337 is loopback-only inside a single-user VM and the *gateway* is the authenticator — same trust argument as the guest agent itself (Phase 3 decision #10). | Lazy start keeps ~200 MB of editor out of RAM for terminal-only sessions (2 GiB VMs) and gives Phase 11's idle logic one less always-on process. Pinning matches the image philosophy; in-VM auto-update would silently drift the fleet. |
| 6 | **Embedding: iframe with partitioned cookies, new-tab fallback, decided by 8.0 evidence.** Task 8.0 builds a minimal two-origin harness and verifies the `Partitioned` cookie flow in current Chrome/Firefox/Safari. If any target browser blocks it, `EditorPanel` opens the editor in a new tab (first-party context — always works) instead of an iframe; the API/cookie design is identical either way. | Third-party-cookie behavior is the one externally-owned risk in this phase — it gets a spike with an explicit fallback rather than discovery during 8.6. Nothing else in the design depends on the outcome. |
| 7 | **PoC file-browser endpoints are removed**: `GET /api/containers/:id/files` and `GET /api/containers/:id/files/read` deleted from `server/index.js`, plus the file-browser window wiring in `public/app.js`. The remaining PoC desktop stays runnable (full replacement is Phase 9's criterion). | The master plan's explicit criterion. Removing only the file endpoints keeps the Phase 9 retirement honest while killing the unauthenticated file-read surface now. |
| 8 | **Deploy: wildcard host handling at both proxy layers.** app-stack nginx gains a `server_name ~^m-…` block proxying everything (Upgrade headers, no buffering — the `/gw/` lesson) to the control plane; `DEPLOYMENT.md`/`RUNBOOK.md` document the NPMplus side: wildcard DNS record `*.<machine-domain>` → app stack, wildcard TLS cert. `PROTEOS_MACHINE_DOMAIN` config on the control plane; absent ⇒ machine-web routing disabled (dev default off except e2e). | Fact #3: without this, every subdomain request returns the SPA with 200 — the exact class of silent failure the `/gw/` gotcha already documented. Making "unset = disabled" keeps non-web deployments unaffected. |

## Wire contracts

### Subdomain auth flow

```
POST /api/machine/web-session            (main origin, requireAuth, CSRF per existing pattern)
  → 200 {"url":"https://m-<uuid>.<dom>/__proteos/auth?token=<hmac>"}   token exp ≤60s
  → 409 machine_not_running · 404 no_machine

GET  https://m-<uuid>.<dom>/__proteos/auth?token=…
  → Set-Cookie: proteos_machine=<signed{machine_id,session_id,exp}>;
                HttpOnly; Secure; SameSite=None; Partitioned; Path=/
  → 302 /
  → 403 bad/expired token

ANY  https://m-<uuid>.<dom>/*            cookie → session alive → ownership → proxy to guest:1025
  → 401 (no/invalid cookie) · 403 (revoked/foreign) · 502 guest_unreachable
  WS upgrades additionally: Origin must equal the subdomain origin; conn registered for
  revocation under the parent session id (close 4001 on logout)
```

### Tunnel port parameter

```
driver:    DialGuest(ctx, machineID string, port uint32) (net.Conn, error)
agentapi:  GET /v1/machines/{id}/guest?port=1025        (default 1024 when absent)
guest:     vsock listener 1025 → raw copy ↔ 127.0.0.1:13337 (start-on-demand, decision #5)
dev:       second unix socket <datadir>/machines/<id>/guest-web.sock; stub backend in tests
```

### Config additions

```
controlplane: PROTEOS_MACHINE_DOMAIN (unset ⇒ machine-web disabled),
              reuses PROTEOS_STATE_KEY for token + cookie signing
guestagent:   PROTEOS_GUEST_WEB_PORT=1025, PROTEOS_CODESERVER_BIN/ADDR (defaults baked)
deploy:       nginx wildcard server block; NPMplus wildcard DNS + TLS (docs)
```

## Package layout (new / touched)

```
controlplane/
  internal/gateway/machineweb.go      # NEW: host routing, auth handler, cookie mint/verify,
                                      #      reverse proxy over tunnel, WS registry hookup
  internal/httpapi/websession.go      # NEW: POST /api/machine/web-session
  internal/httpapi/server.go          # host-first routing wrap
  internal/nodeclient/                # DialGuest port param
nodeagent/
  internal/driver/driver.go           # GuestDialer gains port
  internal/driver/firecracker/guest.go# CONNECT <port> parametrized
  internal/driver/dev/                # per-machine web socket
  internal/httpapi/guest.go           # ?port= validation (1024/1025 allowlist)
guestagent/
  internal/webfwd/                    # NEW: vsock:1025 listener, raw forward, code-server
                                      #      supervisor (lazy start, health, backoff)
image/build-rootfs.sh, manifest.lock  # pinned code-server
web/src/components/EditorPanel.tsx    # NEW: mint → iframe (or tab) embed, state gating
server/index.js, public/app.js        # PoC file endpoints + browser removed
deploy/app-stack/nginx.conf           # wildcard server block
DEPLOYMENT.md, RUNBOOK.md             # wildcard DNS/TLS, gotchas
```

No Postgres migration: the web-session token is stateless HMAC; the subdomain cookie
binds to the existing `sessions` row.

## Tasks (Track A = Mac/dev-driver; Track B = Proxmox/Firecracker)

### 8.0 — Origin-isolation spike + pins (Track A; standalone, do first)
(a) Two-origin harness (two local ports) proving the `SameSite=None; Partitioned` iframe
cookie flow in current Chrome, Firefox, Safari; record results and fix the
iframe-vs-new-tab default (decision #6). (b) Regression test asserting the main session
cookie is host-only and absent from a `m-*.…` request. (c) Pin code-server: version,
sha256, standalone-install layout, verify it runs on the Ubuntu 24.04 base in a
container, flags per decision #5.
**Done when:** browser matrix is written into this plan's PR; the cookie regression test
exists; `PROVIDERS.md`-style pin notes for code-server are recorded for 8.4.

### 8.1 — Tunnel port parameter end-to-end (Track A)
Driver/agentapi/nodeclient changes per the wire contract (port allowlist at the
node-agent route); DevDriver second socket; guest agent `webfwd` listener with a
configurable backend address (tests point it at a stub HTTP+WS server).
**Done when:** dev-stack test dials port 1025 through node-agent → guest and round-trips
HTTP and a WS echo against the stub; port 1024 paths are untouched (existing suites green).

### 8.2 — Gateway machine-web (Track A; after 8.1)
Host-first routing; `/__proteos/auth` + cookie mint/verify; per-request authz; reverse
proxy with upgrade passthrough over the 1025 tunnel; WS conns into the revocation
registry; subdomain-Origin check on upgrades; security headers
(`X-Frame-Options`/CSP `frame-ancestors` allowing only the main origin).
**Done when:** e2e (httptest CP + node-agent DevDriver + stub backend, `*.localhost`):
mint → auth → proxied page + WS echo; authz table — no cookie 401, foreign machine 403,
expired token 403, stopped machine 502/409; logout closes the live proxied WS with 4001;
`/api/*` on the subdomain host 404s; main-host behavior unchanged.

### 8.3 — EditorPanel (Track A; after 8.2)
Mint call + iframe embed (or tab, per 8.0) gated on `running`; reconnect banner on
machine stop (SSE-driven); "Open editor" on MachineCard.
**Done when:** Mac dev stack opens the stub-backed editor frame end-to-end; stopping the
machine surfaces the banner instead of a dead frame.

### 8.4 — code-server bake + supervisor (Track A logic; Track B image; after 8.0)
`webfwd` supervisor per decision #5 (lazy start, health gate, crash backoff — unit-tested
against a fake binary); `build-rootfs.sh` installs pinned code-server; image rebuild +
`PROTEOS_ROOTFS_REF` re-pin; record image-size delta.
**Done when:** a VM from the new image serves real code-server through `DialGuest(1025)`
on the Proxmox host; first request takes the lazy-start path (observed in guest log);
kill -9 of code-server recovers on next request.

### 8.5 — Retire PoC file endpoints (Track A; independent)
Decision #7: delete the two file routes + PoC file-browser window code; PoC desktop
otherwise still boots.
**Done when:** routes return 404, `public/app.js` has no file-browser references, and a
note in the PoC README points at the editor.

### 8.6 — Deploy + live acceptance (Track B; after 8.2–8.4)
nginx wildcard block (Upgrade headers — re-read the `/gw/` gotcha); NPMplus wildcard
DNS + TLS documented in `DEPLOYMENT.md` and applied to the lab deployment; live pass:
open editor from the dashboard, edit + save a workspace file, see it from the terminal
(and vice versa), confirm **no direct route** to the guest (port scan from outside the
host; subdomain without cookie 401), logout in another tab kills the editor session,
DevTools shows the partitioned cookie on the subdomain and the session cookie absent.
Walk the master-plan Phase 8 checklist and tick the boxes in
`plans/proteos-poc-to-prod.md` (the "edits persist" box notes Phase 4 gating if it
hasn't landed).

### Sequencing

```
8.0 ──┬───────────────► (browser matrix gates 8.3's embed mode)
8.1 ──┴──► 8.2 ──► 8.3 ─┐
      8.4 (logic ∥; image Track B) ──┼──► 8.6 (Track B)
      8.5 (independent) ─────────────┘
Buildable immediately in parallel: 8.0, 8.1, 8.5. Only 8.4's image and 8.6 need Proxmox.
```

## Acceptance-criteria mapping (master-plan Phase 8 checklist)

| Criterion | Task |
|---|---|
| code-server reachable **only** via the authenticated gateway | 8.2 (authz table) + 8.6 (outside port scan, cookieless 401) |
| Open/edit/save workspace files through code-server | 8.4 + 8.6 (live) |
| Edits persist on the Phase 4 disk; visible to terminal/agents and vice versa | 8.6 (cross-visibility live; durability gated on Phase 4 — header note) |
| Scoped to the user's workspace; no host/other-tenant access | VM isolation + decision #5 (loopback bind, `/workspace` default); Context interpretation; 8.6 scan |
| Served from a per-machine subdomain, origin-isolated per the master-plan decision | decisions #1–#3, 8.0 (cookie matrix + host-only regression), 8.2, 8.6 (DevTools check) |
| Old PoC file-browser endpoints removed | 8.5 |

## Critical existing files to modify

- `controlplane/internal/httpapi/server.go` — host-first routing; web-session route
- `controlplane/internal/auth/auth.go` — none (cookie already host-only — keep it that
  way via the 8.0 regression test)
- `controlplane/internal/gateway/` — registry reuse (Phase 3 `registry.go`), shared
  origin helpers
- `controlplane/internal/config/config.go` — `PROTEOS_MACHINE_DOMAIN`
- `nodeagent/internal/driver/{driver.go,dev/,firecracker/guest.go}`,
  `nodeagent/internal/httpapi/guest.go` — port parameter + allowlist
- `guestagent/cmd/guestagent/main.go` — webfwd wiring
- `image/build-rootfs.sh`, `image/manifest.lock` — code-server pin; ref re-pin
- `web/src/components/MachineCard.tsx`, `Dashboard.tsx`, `api/client.ts`
- `server/index.js`, `public/app.js` — endpoint removal
- `deploy/app-stack/nginx.conf`, `DEPLOYMENT.md`, `RUNBOOK.md`
- `.github/workflows/ci.yml` — 8.2 e2e job (plain runner)

## Verification

- **Unit/integration (any OS):** token mint/verify (expiry, tamper, wrong machine);
  cookie sign/verify + session binding; host router (subdomain vs main matrix);
  supervisor lifecycle against a fake binary; port allowlist.
- **e2e (Mac, normal CI):** 8.2 — full mint→auth→proxy→WS flow with authz table and
  revocation close; 8.1 — tunnel port round-trip.
- **Live (Proxmox):** 8.6 — real code-server through wildcard DNS/TLS, cross-visibility
  with the terminal, outside-scan negative, logout kill, DevTools cookie audit.
- **CI:** new e2e job on standard runners; existing KVM-gated job re-runs against the
  bigger image (no new gated tests).

## Non-goals / deferred

- **App preview ports** (`m-<uuid>-p<port>` label reserved) — Phase 9+/10; same gateway
  shape, needs per-port policy thought (which ports, what auth).
- **Full PoC desktop retirement** — Phase 9 (`public/` replacement is its criterion;
  8.5 removes only the file endpoints).
- **Editor windowing/multi-window UX** — Phase 9 (EditorPanel is a single panel like
  TerminalPanel).
- **code-server settings sync, extension curation, non-root editor user** — later
  product decisions; defaults this phase.
- **Idle shutdown of code-server** — Phase 11 (lazy *start* ships now; the supervisor is
  where idle-stop will hang).
- **Rate limiting on the subdomain endpoints** — Phase 10 with the rest of `/gw/*`.
- **Multi-instance revocation fan-out for proxied WS** — Phase 10/11 with the Phase 3
  registry's own multi-instance story.

## As-built notes

### 8.0(a) — Embedding decision (browser matrix)

`EditorPanel` ships **iframe-by-default with an always-present "Open in new tab"
fallback** — both work from the same partitioned-cookie design, so no browser is
blocked regardless of the matrix outcome. The subdomain cookie is
`HttpOnly; Secure; SameSite=None; Partitioned` (CHIPS). Current-2026 behavior:
Chrome 114+ honours `Partitioned` for the embedded cross-origin frame; Firefox
and Safari partition third-party cookies by default (Total Cookie Protection /
ITP), so the frame carries its own cookie under the dashboard's top-level site.
A browser that blocks the framed cookie still has the first-party new-tab path
(always works). The cookie is `Secure`, so the editor requires HTTPS — the
`PROTEOS_COOKIE_SECURE=true` deployment default already enforces this.

### 8.0(c) — code-server pin

code-server is baked from the **standalone GitHub release tarball** (it bundles
its own Node, so it is self-contained and needs no system Node):
`build-rootfs.sh` installs it to `/usr/local/lib/code-server` with
`/usr/local/bin/code-server` symlinked, on by default (`--no-codeserver` opts
out). The version + tarball sha256 are recorded in `manifest.lock`
(`codeserver_version`, `codeserver_sha256`); unpinned ⇒ the latest release, a
forced rebuild bumps it (Ansible `proteos_codeserver_version`). Flags per
decision #5 (`--auth none --bind-addr 127.0.0.1:13337 --disable-telemetry
--disable-update-check`, user-data/extensions under `$HOME`, default folder
`/workspace`). Image-size delta: **~+350–400 MiB** (recorded in `manifest.lock`
`image_size_mib` after a real bake).

### Verification status

- **Built + tested on a Mac (Track A):** tunnel port param (`go test` node-agent +
  the webfwd supervisor/forward unit tests); the full machine-web e2e
  (`TestMachineWebE2E`: real node-agent DevDriver + guest webfwd → stub backend,
  mint→auth→proxy→WS-echo→logout-closes-WS, `/api` on subdomain 404, host-first
  routing); the gateway authz table; the cookie host-only regression
  (`TestSessionCookieIsHostOnly`); web `tsc`/`vite build`.
- **Host-only (Proxmox), scripted for the operator:** image bake (the
  `build-rootfs.sh` / Ansible code-server step), `image/verify-phase8-rootfs.sh`
  (loop-mount checks), `image/verify-phase8-live.sh` +
  `TestGuestWebForwardCodeServer` (boot a VM, dial port 1025, lazy-start
  code-server), and the browser live-acceptance walkthrough in RUNBOOK Part G.
