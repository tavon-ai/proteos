# Plan: Port Preview — run an app in your machine, reach it via URL

> Source: backlog item *"users would like to run apps in their microVMs and have
> them exposed via url"* + in-conversation design (2026-06-19).
> Off the master-plan 1–12 numbering (feature work, like `machine-templates` and
> `multi-machine`); stages labelled **PP1–PP4** to avoid colliding with the
> master-plan phase numbers. Layers on top of master-plan **Phase 8** (machine-web)
> and the **multi-machine** work.

## Scope

Let the **logged-in machine owner** open a web app / dev server running on a port
inside their own Firecracker machine, in the browser, for testing. **Private only**
— owner-authenticated, **not** public/anonymous/shareable. Public sharing is an
explicit non-goal here (see Non-goals) because it changes the security posture
(anonymous reach into a user VM, abuse surface, and ideally a separate registrable
domain for untrusted content) and should be its own future effort.

## Context

This extends the Phase 8 machine-web plane rather than building anything new at the
edge. The machine-web reverse proxy (`controlplane/internal/gateway/machineweb.go`),
the vsock guest tunnel (node-agent `RouteGuest` + `DialGuest`), the wildcard
`*.machines.<domain>` DNS/TLS at NPMplus, and the token→cookie auth flow all already
exist and are reused. The only genuinely new component is a generic guest-side
forwarder that bridges the tunnel to an arbitrary loopback port inside the VM.

The `m-<uuid>-p<port>` preview host form is **already reserved**: `parseHost` and
`machineLabelRe` (`machineweb.go:48-52, 157-174`) deliberately reject it today, with
a comment marking it for "Phase 9+". This feature is the thing that claims it.

Per the multi-machine work, a user owns N machines; the preview unit is therefore a
`(machine, port)` coordinate, not a user — which is exactly what the reserved host
form encodes. A per-user proxy domain was considered and rejected: it would need a
mutable per-user route table and would sit in the apex wildcard zone overlapping the
main app, whereas `m-<uuid>-p<port>.machines.<domain>` reuses the existing wildcard
cert with no new DNS/TLS and carries the route in the URL itself.

## Architectural decisions

Durable decisions that apply across all stages:

- **Preview host pattern**: `m-<uuid>-p<port>.machines.<PROTEOS_MACHINE_DOMAIN>`.
  Single DNS label (≤63 chars: `m-` + 36-char uuid + `-p` + ≤5 digits ≈ 45). Covered
  by the existing `*.machines.<domain>` wildcard DNS + TLS — **no new cert or DNS
  zone**. The editor keeps its current port-less `m-<uuid>.<domain>` host.
- **Origin isolation per `(machine, port)`**: each preview host is its own origin.
  The machine cookie stays **host-only** (set with no `Domain` attribute, as today),
  so a cookie minted for one preview host is never sent to another host or to the
  editor. **Do NOT add `Domain=.machines.<domain>`** — that would let one machine's
  app read another's cookie. Each preview origin gets its own mint→auth handshake.
- **Auth**: reuse the Phase 8 flow unchanged — main origin mints a ≤60s HMAC
  web-session token; the preview origin's `/__proteos/auth` exchanges it for a
  signed, subdomain-scoped, `SameSite=None; Partitioned; Secure; HttpOnly` cookie
  bound to the parent session id; every request re-checks cookie → parent session
  alive → owner still owns the machine → machine running. The token/mint carries an
  optional **target port**. No anonymous/public access path.
- **Port policy**: previewable ports are a configurable range (env, e.g.
  `PROTEOS_PREVIEW_PORT_MIN`/`MAX`), default high range (e.g. 1026–65535). `1024`
  (terminal) and `1025` (code-server) stay reserved and rejected as preview ports.
  Out-of-range ⇒ clean `400`/`bad_request` before any dial (extend
  `agentapi.ValidGuestPort`).
- **Guest forwarder**: a generic loopback forwarder reachable only over the per-VM
  private vsock transport; for a requested port P it dials `127.0.0.1:P` inside the
  guest and raw-bridges bytes. **No supervisor** (unlike code-server) — the user's
  own process is the backend; a missing backend drops the connection → gateway maps
  it to `502`, exactly like an unreachable guest. Does no auth of its own (the
  gateway is the authenticator). It ships in the `guestagent` binary, so it requires
  a **rootfs rebuild + re-pin `PROTEOS_ROOTFS_REF`** (the SHA-keyed rootfs auto-
  rebakes on a guest-agent source change, same as Phase 8).
- **Tunnel target port**: node-agent `RouteGuest` already takes `?port=`
  (`GuestPortParam`) and relays opaquely; the control-plane reverse proxy stops
  hardcoding `GuestWebPort` (1025) and passes the requested preview port through.

---

## Stage PP1: End-to-end tunnel for one preview port (tracer bullet)

**User story**: US1 — owner opens an in-machine web app in the browser. US3 — only
the owner can reach it.

### What to build

The narrowest complete path through every layer for a **single** preview port, proven
end-to-end before generalizing:

- `parseHost` accepts the `m-<uuid>-p<port>` form and returns `(machineID, port)`;
  the reverse-proxy dial threads that port into `DialGuest` instead of the hardcoded
  `GuestWebPort`.
- The web-session mint (`POST /api/machine/web-session`) accepts an optional target
  port and binds it into the token; the minted URL builds the `-p<port>` host.
- The node-agent allowlist temporarily admits one preview port so the tunnel opens.
- A generic guest forwarder bridges the tunnel to `127.0.0.1:<port>` inside the VM
  (no supervisor).
- The app-stack nginx `server_name` regex accepts the optional `-p<port>` suffix and
  still proxies to the control plane.

No UI in this stage — exercised via a manually-minted web-session URL.

### Acceptance criteria

- [x] A logged-in owner, given a minted web-session URL for `m-<uuid>-p<port>`, loads
      content served by a process listening inside that VM, in the browser.
      *(Live e2e `TestMachineWebE2E` reaches two loopback apps through the real tunnel.)*
- [x] The auth handshake (token → cookie → proxied request) succeeds end-to-end over
      the preview host; an unauthenticated request to the preview host is rejected.
- [x] A request for a port with **no** listener inside the VM yields `502`, not a hang.
      *(Forwarder drops the conn → gateway `ErrorHandler` 502.)*
- [x] `parseHost` unit tests cover the `-p<port>` form (valid, missing port, junk port)
      and still reject the bare/foreign forms.
- [x] Existing Phase 8 editor flow (port-less `m-<uuid>` host) is unchanged and green.

---

## Stage PP2: Configurable, bounded port range

**User story**: US2 — preview several different ports. US4 — operator-bounded range,
reserved system ports stay reserved.

### What to build

Generalize the single-port tracer into a real, bounded policy:

- `agentapi.ValidGuestPort` (and the control-plane mint validation) accept any port in
  the configured preview range, keep `1024`/`1025` reserved, and reject everything else
  with a clean `400`/`bad_request` before dialing.
- Range bounds are configurable via env, with a sane default high range.
- Confirm token + cookie are scoped per `(machine, port)` origin: a cookie minted for
  one preview port is rejected on a different preview port and on the editor host
  (host-only cookie, no domain widening).

### Acceptance criteria

- [x] Two distinct in-range ports of the same machine are independently reachable by
      the owner, each via its own mint→auth handshake. *(Live e2e two-port leg.)*
- [x] Reserved ports (1024, 1025) and out-of-range ports are rejected at mint time and
      at the node-agent allowlist with a clean error, no dial attempted.
- [x] A preview cookie for port A returns `401`/unauthorized when replayed against the
      port-B host or the editor host.
- [x] Range bounds honor the env config; defaults documented.
      *(`PROTEOS_PREVIEW_PORT_MIN/MAX` on both binaries; `.env.example`s + RUNBOOK Part H.)*
- [x] Unit/integration tests cover in-range, reserved, and out-of-range ports.

---

## Stage PP3: MachineCard UI

**User story**: US1, US2 — an owner-facing way to open and switch preview ports.

### What to build

An "Open app on port…" affordance on the MachineCard: the owner enters a port, the SPA
calls the web-session mint endpoint with that port, and opens the returned preview URL.
Mirror the existing `EditorPanel` UX — open in a new tab with an iframe fallback, and
show a "machine stopped / reconnect" banner when the preview origin reports the machine
down. Scope the affordance to the active machine (multi-machine switcher).

### Acceptance criteria

- [x] From the MachineCard, the owner enters a port and the in-machine app opens in the
      browser without manual URL construction. *(Taskbar "Open app…" → preview window.)*
- [x] Switching to a different port re-mints and opens the new preview origin; the prior
      preview is unaffected. *(Dedupe per `(machine, port)`; unit-tested.)*
- [x] Stopping the machine surfaces a clear "machine stopped" state in the preview UI,
      consistent with the editor's behavior.
- [x] Frontend type-checks/lints clean; no regression to the editor "Open editor" button.
      *(Separate `previewSession` client method; editor path untouched.)*

---

## Stage PP4: Hardening + infra/acceptance

**User story**: US3 — logout/stop/destroy kills the preview. US4 — operator/infra.

### What to build

Security and operational closure plus the Proxmox-only steps:

- WebSocket Origin check and the revocation registry cover preview hosts exactly as
  they cover the editor: a preview WS upgrade must originate from its own `(machine,
  port)` origin, and logout/session-revoke closes live preview sockets.
- Rebuild the rootfs with the generic guest forwarder and re-pin `PROTEOS_ROOTFS_REF`
  (Ansible `node_agent` role auto-rebakes on the guest-agent source change); loop-mount
  + live verify the forwarder is present and reachable on a booted VM.
- Confirm NPMplus and the app-stack nginx wildcard handle the longer `-p<port>` label
  (longest-label / 63-char sanity check).
- Runbook entry + browser live-acceptance on the Proxmox/KVM host: boot a VM, run a dev
  server inside it, reach it from a browser as the owner, then verify logout and machine
  stop both kill the preview.

### Acceptance criteria

- [x] A preview WS upgrade from a foreign origin is rejected (`origin_forbidden`).
      *(`TestMachineWebPreviewWebSocketOriginAndRevocation`.)*
- [x] Owner logout and `session_revoked` close any open preview WebSocket immediately;
      machine stop yields the stopped state, not a hang. *(Same test: revoke closes the
      live preview socket; `machine_stopped` is a synchronous 502, shared with the editor.)*
- [~] Rootfs rebuilt and `PROTEOS_ROOTFS_REF` re-pinned; forwarder verified present in
      the baked image and reachable on a booted VM. **Operator step** — source is ready
      (systemd unit pins `vsock:1026`; `verify-phase8-rootfs.sh` now asserts it), but the
      SHA-keyed bake + re-pin must run on the KVM host (cannot bake in this environment).
- [~] NPMplus/nginx confirmed serving the full `m-<uuid>-p<port>.machines.<domain>`
      host over TLS. **Operator step** — nginx `server_name` accepts the `-p<port>` label
      and the 45-char longest-label fits the wildcard cert (RUNBOOK Part H4); live TLS
      check is on the Proxmox host.
- [~] Live-acceptance checklist passes on Proxmox: dev server reachable by owner;
      logout and stop both terminate access. **Operator step** — checklist authored
      (RUNBOOK Part H3); runbook + `.env.example`s updated. The browser walkthrough runs
      on the Proxmox/KVM host.

## Sequencing

```
PP1 (tracer) ──► PP2 (port range) ──► PP3 (UI) ──┐
                                                  ├──► PP4 (harden + live)
                            (PP3 UI can start once PP2's mint accepts a range;
                             PP4 gathers security + the Proxmox-only steps)
```

## Non-goals / deferred

- **Public / anonymous / shareable previews.** This feature is owner-authenticated
  only. Public sharing needs anonymous access, abuse controls, and — critically —
  untrusted content on a **separate registrable domain** (not a sibling subdomain of
  the auth domain), so it is a distinct future effort, not a flag on this one.
- **Persistent / declared port config per machine.** Ports are reached on demand by
  URL; there is no stored per-machine "exposed ports" list in this scope.
- **Multiple simultaneous proxied protocols beyond HTTP/WS.** The forwarder is a raw
  byte bridge, but the auth/origin model is built around HTTP(S)+WebSocket browser
  testing; arbitrary TCP exposure is out of scope.
- **Rate limiting on the preview path** — folds into the master-plan Phase 10
  hardening pass with the rest of `/api/*` and `/gw/*`.
```
