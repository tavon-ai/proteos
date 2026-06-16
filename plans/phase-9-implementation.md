# Phase 9 Implementation Plan: React desktop UX

> Source: `plans/proteos-poc-to-prod.md` Phase 9, planned 2026-06-16.
> Status: **not started.**
> Prerequisites: Phases 1–8 landed. Phase 3 (terminal gateway + guest sessions),
> Phase 5/6 (provider registry + agent sessions over `/gw/agent`), Phase 7
> (control channel + clone + repos APIs), and Phase 8 (code-server through the
> machine-web subdomain) are all hard dependencies — this phase is where they
> **converge into one product surface**. Phase 4 (persistent disk) is assumed for
> the "layout/projects survive a stop/start" criteria; where it has not landed on
> a given stack, the desktop still works but loses cloned projects and saved
> layout on stop (same gating note Phases 5–8 carried — annotate the live walk).

## Context

This is the first phase whose deliverable is **the product, not a capability**.
Phases 1–8 each shipped a vertical slice exposed as a single panel or modal on a
flat dashboard (`web/src/routes/Dashboard.tsx`: `MachineCard` + `ReposPanel` +
`ProvidersPanel`, with terminal/editor/agent opening as mutually-exclusive
fixed-overlay modals — `TerminalPanel.tsx`, `EditorPanel.tsx`). The pieces all
exist and are individually green; what is missing is the **workflow that ties
them together** and the **multi-window desktop** the master plan calls for.

The user-facing throughline, decided 2026-06-16, is **project-centric**: clone a
GitHub repo, then open a coding agent *in that repo's folder*, or the editor *in
that repo's folder*, or a terminal *there* — as many windows as you like, side by
side. The desktop is organized around projects (cloned repos under `/workspace`),
with free-floating windows layered on top.

Four repo facts anchor the design:

1. **Agents and terminals already run as PTY sessions through the gateway**
   (`/gw/terminal`, `/gw/agent/{provider}`), and the guest session struct already
   has a `Dir` field (`guestagent/internal/term/session.go` — "the working
   directory the session starts in"). But today `Dir` is only ever set to the
   unprivileged user's `$HOME`; there is **no path from the browser to "start this
   session in `/workspace/<repo>`."** That plumbing is the central backend work of
   this phase.
2. **Session identity is a name** constrained to `^[a-z0-9-]{1,32}$`
   (`guestwire.ValidSessionName`), and for agents the **whole remainder after
   `agent-`** is taken as the provider key (`guestwire.ProviderKeyFromSession`).
   So `agent-claude-myrepo` would resolve provider `claude-myrepo` — broken.
   Project/window scoping therefore **cannot live in the session name**; provider
   and working directory must travel as their own handshake parameters, and the
   session name becomes an opaque, stable, per-window identifier (the handle the
   saved layout reconnects to — Phase 3's tmux-like "reconnect to the same shell"
   is what makes restored windows resume their live PTY).
3. **The machine SQLite `kv` table already exists and is reserved for exactly
   this** (`guestagent/internal/persist/persist.go` — `Set`/`Get`, comment:
   "Reserved for window layout, session index, preferences"). The data model in
   the master plan puts window layout + project/repo metadata + per-machine
   preferences in machine SQLite. Phase 9 is its first consumer.
4. **The guest control channel is the right transport for machine-local state**
   (`internal/guestctl` ↔ `guestagent/internal/ctlchan`): a CP-dialed,
   bidirectional, authorized-at-one-choke-point JSON request/response channel
   (Phase 7). Listing projects on disk and reading/writing the layout `kv` are new
   ops on that channel — no new transport, the same per-machine authority spine.

One master-plan criterion needs an explicit interpretation. **"Window state
(layout) persists via the machine SQLite where appropriate"** — *where
appropriate* is doing work. Window layout and open-session handles are
machine-bound (a window is a live connection to a running VM; it is meaningless
without the machine), so they go in SQLite, read/written over the control
channel. **Theme and wallpaper are a viewing preference of the browser**, not
machine state, and must render correctly *before* the machine is running (when
SQLite is unreachable), so they stay in `localStorage`. This split is decision #6.

The PoC desktop (`public/app.js`, 1299 lines) is the **behavioral reference** for
the window manager — cascade placement, header-drag, bottom-right resize, z-order
on focus, minimize-to-dock, maximize-with-restore — and is **fully retired** by
this phase (master-plan criterion: "PoC `public/` desktop is fully replaced").
The PoC Express server (`server/index.js`) and its remaining endpoints
(`/api/containers/*`, `/api/wallpapers`, `/api/settings/api-keys`, …) go with it —
the Go control plane + React SPA is the whole application after this phase.

## Architecture decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | **Project-centric desktop shell.** The desktop's primary surface is a **Projects launcher**: each cloned repo under `/workspace` is a tile (name, branch, clean/dirty, last commit) with actions `[Open editor]`, `[Open agent ▾]` (provider menu), `[Open terminal]` — each scoped to that project's folder. A top **taskbar** carries machine state + start/stop + a clock; a **dock** lists open windows (focus/restore); a **Settings** window holds provider keys, GitHub connection, and the clone form. Free-floating windows (terminal, editor, agent, logs, settings, projects) layer on top with cascade placement. | The user's stated workflow ("clone → open agent/editor in the repo folder") made structural: the project, not the window, is the unit of work, so the UI leads with projects and every window is born already pointed at one. Matches the chosen mock exactly. |
| 2 | **Thin custom window manager + `react-rnd` for drag/resize only.** A `WindowManager` context owns the window registry (id → {kind, title, projectId, session, geometry, state, zIndex}), z-order on focus, minimize/maximize/restore/close, cascade placement, and layout (de)serialization. `react-rnd` provides only the drag/resize handles (pointer-capture, edge cases) under our `<Window>` chrome. **Window content components mount once and never unmount until closed** — minimize/maximize/restore are CSS (`display`/geometry) changes, *not* React remounts. | The one dependency that is genuinely fiddly (pointer capture, resize math) is delegated to a battle-tested lib; everything stateful (and persisted) stays ours. The mount-once rule is load-bearing: remounting a `<Terminal>` drops its WebSocket (survivable — Phase 3 reconnect) and remounting the editor `<iframe>` **reloads code-server** (jarring, loses cursor/unsaved buffers). Reordering windows must never touch their React subtree. |
| 3 | **Working directory + provider become explicit handshake parameters.** `/gw/terminal` and `/gw/agent/{provider}` gain `?cwd=<abs path>` (and `/gw/terminal` keeps `?session=`); the **session name is now an opaque per-window id**, no longer the carrier of provider or scope. The gateway forwards `cwd` (and, for agents, the provider) to the guest in the session-creation handshake; the guest sets `term.Config.Dir = cwd`. **`cwd` is validated twice**: control plane checks it is one of the machine's listable projects (decision #4); guest canonicalizes, requires the `/workspace/` prefix, and requires an existing directory (defence in depth — the guest is untrusted but acts only with the owner's authority, and the owner already has a shell). Absent/empty `cwd` ⇒ existing behavior (session user `$HOME`). | Removes the only blocker to the whole phase's UX. Decoupling provider/scope from the session name (fact #2) keeps Phase 3's stable-name reconnect intact while allowing many concurrent agents/terminals in different folders. Double validation keeps "scoped to the user's workspace" honest without pretending the guest is a trust boundary it isn't. |
| 4 | **Projects API = `projects.list` over the control channel; disk is the source of truth.** New CP→guest op `projects.list` → guest scans `/workspace` for git repos and returns `[{name, path, remote, branch, dirty, last_commit_at, last_commit_msg}]`; new `GET /api/projects` relays it (machine must be running, else `409 machine_not_running`). Clone completion already arrives as a `git.clone` `machine_event` over SSE (Phase 7) — the desktop refetches projects on that event. SQLite stores only **per-project UI metadata** (last-opened, saved per-project layout), never the project list itself. | After Phase 4 a clone survives restarts and a repo can be deleted from a terminal, so the filesystem — not a clone-event log or a stale table — is the only correct answer to "what projects exist." Reusing the SSE `git.clone` signal avoids a polling/job API. One new authorized op, same Phase 7 choke point. |
| 5 | **Editor opens at the project folder via a `folder` carried through the web-session mint.** `POST /api/machine/web-session` gains optional `{folder}`; `MintWebSessionURL` embeds it in the one-shot token; `/__proteos/auth` redirects to `/?folder=<path>` (code-server's native open-folder query) instead of `/`. `folder` is validated against the listable project set (same as `cwd`). Absent ⇒ today's behavior (workspace root). | code-server already supports `?folder=`; the only work is carrying a validated path through the existing Phase 8 mint→auth→redirect chain so "open the editor in the repo folder" lands on the repo, not the workspace root. No change to the proxy or cookie model. |
| 6 | **Persistence split: layout + sessions → SQLite (over the control channel); theme + wallpaper → `localStorage`.** New CP→guest ops `kv.get`/`kv.set` (thin wrappers over `persist.Get/Set`) under a `desktop.layout` key; new `GET /api/machine/desktop` + `PUT /api/machine/desktop` relay them. The React desktop loads layout when the machine reaches `running`, restores windows (reconnecting each session by its stored opaque name), and **debounce-saves** (≈1 s) on window create/move/resize/close. Theme (dark only for now) and wallpaper choice live in `localStorage` so the shell paints correctly while the machine is `stopped`/booting. | "Where appropriate" resolved (Context): machine-bound state follows the machine via SQLite; pure viewing chrome must survive a stopped machine, so it stays client-side. The control channel already carries machine-local state with the right authority; `kv` is purpose-built. Debounce keeps a drag from hammering the channel. |
| 7 | **Settings, GitHub, and clone fold into desktop windows; the flat dashboard is deleted.** `ProvidersPanel`, `GitHubStatus`, and `ReposPanel` (the clone form) are recomposed into a **Settings window** (Providers tab, GitHub tab) and the Projects launcher's `[+ Clone repo]` action. `MachineCard`'s controls move to the taskbar; its event log becomes a **Logs window** (the `machine_events` SSE stream). `routes/Dashboard.tsx` is replaced by the desktop shell; `Login.tsx` and the `RequireAuth` gate are unchanged. | The Phase 5–8 components are reused, not rewritten — they move from panels into windows. Machine lifecycle and the event stream become first-class desktop chrome (taskbar + Logs window), satisfying the master-plan criteria without inventing new APIs. |
| 8 | **Retire the PoC fully.** Delete `public/` (desktop + `app.js` + assets) and `server/` (Express PoC + all remaining routes); drop their references from build/run scripts and docs. The React SPA served behind the control plane (and the dev `vite`) is the only UI. Wallpaper assets we keep are moved into `web/public/`. | Master-plan criterion ("PoC `public/` desktop is fully replaced") plus Phase 8's deferred server cleanup. Leaving a second unauthenticated server running is a standing risk; this phase is the right place to remove it. |

## Wire contracts

### Control channel (new ops; CP-dialed WS at guest `GET /control`, Phase 7 frame shape)

```
frame: {"id":N,"kind":"req|resp|err","op":"…","payload":{…}}   (unchanged)

CP → guest:
  projects.list   {}  →  resp {"projects":[
                          {"name":"ProteOS","path":"/workspace/ProteOS",
                           "remote":"https://github.com/tavon/ProteOS.git",
                           "branch":"main","dirty":false,
                           "last_commit_at":"<RFC3339>","last_commit_msg":"…"}…]}
  kv.get          {"key":"desktop.layout"}        → resp {"value":"<json string>"|null}
  kv.set          {"key":"desktop.layout","value":"<json string>"} → resp {"ok":true}
(authz: machine id from the dial → owner; ops are read/write of THIS machine's
 own disk only, same choke point as git.* — no payload-supplied identity)
```

### Gateway (terminal + agent) — new/changed query params

```
WS /gw/terminal?machine=<uuid>&session=<opaque>&cwd=/workspace/<repo>
   session: ^[a-z0-9-]{1,32}$, opaque per-window (stable across reconnects)
   cwd: optional; validated ∈ listable projects (CP) and under /workspace (guest)
WS /gw/agent/<provider>?machine=<uuid>&session=<opaque>&cwd=/workspace/<repo>
   provider from path (unchanged); session opaque (NO longer "agent-<provider>");
   gateway tells the guest "spawn provider <p>, Dir=<cwd>" in the handshake
errors (pre-upgrade, unchanged set): 401 · 403 origin_forbidden ·
   404 no_machine/unknown_provider · 409 machine_not_running/no_provider_key ·
   400 bad_cwd (new: cwd not a listable project) · 502 guest_unreachable
```

### Control-plane HTTP (new)

```
GET  /api/projects                 → 200 {"projects":[…as above…]}
                                   · 409 machine_not_running · 502 guest_unreachable
GET  /api/machine/desktop          → 200 {"layout":<json>|null}
PUT  /api/machine/desktop  {layout}→ 204 · 409 machine_not_running
POST /api/machine/web-session      → body now optional {"folder":"/workspace/<repo>"}
                                     (Phase 8 shape otherwise unchanged; 400 bad_folder
                                      if folder ∉ listable projects)
```

### Config additions

```
none required on the control plane (reuses PROTEOS_MACHINE_DOMAIN, control channel,
  PROTEOS_STATE_KEY); guest reuses persist + /workspace conventions.
web: VITE_ build already serves the SPA; wallpaper assets move to web/public/.
```

## Package layout (new / touched)

```
controlplane/
  internal/guestctl/manager.go      # + projects.list, kv.get, kv.set ops (relay helpers)
  internal/guestctl/frames.go       # + payload types for the three new ops
  internal/httpapi/projects.go      # NEW: GET /api/projects (relay projects.list)
  internal/httpapi/desktop.go       # NEW: GET/PUT /api/machine/desktop (relay kv.*)
  internal/httpapi/websession.go    # + optional {folder}; validate ∈ projects
  internal/httpapi/gateway.go       # + ?cwd= parse/validate for /gw/terminal & /gw/agent
  internal/httpapi/agent.go         # provider stays in path; session→opaque; cwd→guest
  internal/gateway/terminal.go      # ProxyOpts gains Cwd; forward to guest handshake
  internal/gateway/machineweb_token.go # token carries folder; auth redirect → /?folder=
guestagent/
  api/guestwire.go                  # + cwd field on session handshake; provider field;
                                    #   + OpProjectsList/OpKVGet/OpKVSet consts + payloads
  internal/ctlchan/                 # + projects.list (scan /workspace+git), kv.get/set
  internal/term/manager.go          # accept requested Dir (validated) at session create
  internal/server/                  # terminal/agent WS: read cwd+provider, guard path
  internal/persist/persist.go       # (already has Get/Set; add layout key helper if useful)
web/src/
  desktop/WindowManager.tsx         # NEW: context, registry, z-order, layout (de)serialize
  desktop/Window.tsx                # NEW: react-rnd chrome (header, min/max/close, resize)
  desktop/Desktop.tsx               # NEW: shell — taskbar, dock, wallpaper, window layer
  desktop/Taskbar.tsx               # NEW: machine state badge + start/stop + clock
  desktop/Dock.tsx                  # NEW: open-window list / restore
  desktop/ProjectsLauncher.tsx      # NEW: project tiles + actions + [+ Clone repo]
  windows/TerminalWindow.tsx        # wraps existing <Terminal> (cwd+session aware)
  windows/EditorWindow.tsx          # wraps EditorPanel iframe (folder-aware mint)
  windows/AgentWindow.tsx           # <Terminal> against /gw/agent (provider+cwd)
  windows/LogsWindow.tsx            # machine_events SSE log (from MachineCard)
  windows/SettingsWindow.tsx        # Providers + GitHub tabs (ProvidersPanel/GitHubStatus)
  api/client.ts, api/hooks.ts       # + projects, desktop layout, folder param, cwd builders
  lib/terminalSocket.ts             # terminalURL/agentURL gain cwd + opaque session
  (DELETED) routes/Dashboard.tsx, components/MachineCard.tsx, ReposPanel.tsx as panels
            → recomposed into windows above
public/        → DELETED   server/ → DELETED
deploy/, RUNBOOK.md, DEPLOYMENT.md, run scripts → drop PoC server references
```

No new Postgres migration: projects come from disk over the channel; layout lives
in machine SQLite; theme/wallpaper in `localStorage`.

## Deployment / host artifacts (Ansible) — what lands on the Firecracker host

A standing rule on this project (learned the hard way): **anything that runs on
the Firecracker/KVM host must be in the Ansible playbook** (`deploy/ansible/`), or
it works on a hand-set-up box and silently breaks on a clean provision. Phase 9 is
deliberately light here, but the audit must be explicit rather than assumed.

- **Only the guest agent is new host-resident code.** The `projects.list`/`kv.*`
  control ops and the `cwd` session handling live in `guestagent`, which the
  `node_agent` role already bakes into the rootfs via `image/build-rootfs.sh`. The
  baked image and `PROTEOS_ROOTFS_REF` are **keyed to the source git short SHA**
  (`proteos-rootfs-<base>-ga<sha>.ext4`; `roles/node_agent/tasks/main.yml:83,198,213`)
  and the bake is guarded on `src_sync.changed` (`:201`). So a guest-agent change
  **auto-rebakes and re-pins** on the next playbook run — no forced
  `manifest.lock` delete (that caveat only applies to version-var bumps *without*
  a source change, e.g. code-server).
- **No new packages, no new `group_vars/all.yml` block.** Unlike Phases 5–8 (each
  added a provider/git/code-server install + var block), Phase 9 adds nothing to
  the rootfs: `projects.list` reuses the Phase 7 git binary; `kv` uses the
  in-guest pure-Go SQLite (`persist.go`); `cwd` needs nothing on the host.
- **node-agent is unchanged.** `cwd` is a query parameter on the WS *inside* the
  existing tunnel to an already-allowlisted guest port (no new port). It is
  rebuilt from source by the role regardless (`:65–77`).
- **The control plane and React SPA are NOT on this host** — they deploy via the
  app-stack VM path (separate from this playbook). Most of Phase 9 never touches
  the Firecracker host.
- **The one playbook change to make:** extend the **KVM acceptance integration
  suite** the role runs as its pre-serve gate (`go test -tags firecracker …`,
  `:273–289`) to exercise a `cwd`-scoped session and a `projects.list`/`kv`
  round-trip. Phases 7/8 added *loop-mount* artifact-presence gates
  (`verify-phaseN-rootfs.sh`); Phase 9 adds **no baked artifact**, so its checks
  are runtime and belong in the boot-a-VM suite, not a loop-mount gate. This is
  the thing easy to miss: a host that bakes a broken guest agent must be caught
  before it is green-lit to serve.
- **Persistent-disk corollary.** Layout persistence (`kv`) writes to the machine
  SQLite on the Phase 4 disk. With the disk present (`persist=disk`) it survives
  stop/start; on a stack in `persist=none`/`dir`, `kv.set` is a **no-op**
  (`persist.go` returns no-op when the db is nil) and layout does not persist —
  same gating note as the projects/clone criteria.

## Tasks (Track A = Mac/dev-driver; Track B = Proxmox/live)

### 9.0 — Window manager + desktop shell skeleton (Track A; foundational)
Build `WindowManager` (registry, z-order, focus, min/max/restore/close, cascade)
and `<Window>` (react-rnd chrome). Mount-once invariant (decision #2). `Desktop`
shell with taskbar (machine state + start/stop + clock from existing
hooks/SSE), dock, wallpaper background. Render one placeholder window kind to
prove drag/resize/min/max/z-order/close and that reordering never remounts content
(assert via a render-counter in the placeholder).
**Done when:** desktop renders behind the existing auth gate; multiple placeholder
windows open, drag, resize, minimize-to-dock, maximize+restore, close, and
focus-raises-z; a moved/refocused window's content does **not** remount (test);
machine start/stop works from the taskbar; `vitest`/`tsc`/`vite build` green.

### 9.1 — Working-directory + provider session params end-to-end (Track A; backend)
Decision #3: `?cwd=` on `/gw/terminal` and `/gw/agent`; opaque session names;
provider stays in the agent path; gateway forwards cwd (+provider) to the guest
handshake; `term.Config.Dir` set from validated cwd; guest path guard
(`/workspace/` prefix + exists); CP validates cwd ∈ listable projects (stub the
project set in this task's tests, real in 9.2). Multiple concurrent sessions with
distinct opaque names in distinct dirs.
**Done when:** dev-stack e2e — two terminals with different `cwd` each land in the
right directory (`pwd` over the PTY); an agent session launches with `cwd` set;
bad cwd (outside `/workspace`, nonexistent, or not listable) → `400 bad_cwd` and
no session; existing `session=main`/no-cwd paths unchanged (Phase 3/5 suites green).

### 9.2 — Projects API (Track A; backend; parallel with 9.1)
Decision #4: guest `projects.list` (scan `/workspace`, read git remote/branch/
dirty/last-commit per repo); `internal/guestctl` relay; `GET /api/projects`;
wire cwd/folder validation (9.1, 9.5) to this listable set. Desktop refetches on
the `git.clone` SSE event.
**Done when:** dev-stack e2e — clone two repos (Phase 7 harness) → `GET /api/projects`
lists both with branch/dirty; deleting a repo dir removes it from the list; a
non-git dir under `/workspace` is excluded; `409` when the machine isn't running.

### 9.3 — Desktop state persistence (Track A; backend + React; parallel)
Decision #6: guest `kv.get`/`kv.set`; `GET`/`PUT /api/machine/desktop`; React
loads layout on `running`, restores windows (reconnect by stored session name),
debounce-saves on change; theme/wallpaper in `localStorage`.
**Done when:** open three windows, move them, reload the page → same windows at the
same geometry, terminals reconnected to their live PTYs with scrollback (Phase 3);
layout survives a stop/start on a Phase-4 disk; theme/wallpaper persist with the
machine stopped (SQLite unreachable) via `localStorage`.

### 9.4 — Project-centric launcher + scoped window types (Track A; after 9.0–9.2)
`ProjectsLauncher` tiles + actions; `TerminalWindow`/`AgentWindow` build their WS
URL with the project `cwd` and a fresh opaque session; `EditorWindow` mints a
web-session with `{folder}` (decision #5). `[Open agent ▾]` lists enabled
providers with a key set (reuse `useProviders`). Window titles carry the project
(`Claude — ProteOS`).
**Done when:** dev-stack — from a project tile, Open editor (lands on the repo
folder), Open terminal (shell in the repo), Open agent→Claude (CLI cwd in the
repo) each open a window correctly scoped; multiple projects' windows coexist;
opening an agent for a project without a provider key routes to Settings.

### 9.5 — Settings/GitHub/clone/logs as windows; taskbar controls (Track A; after 9.0)
Decision #7: `SettingsWindow` (Providers + GitHub tabs from `ProvidersPanel`/
`GitHubStatus`); clone form on the launcher (`ReposPanel` logic) with grants_url
links and reconnect banner; `LogsWindow` from `MachineCard`'s event log; taskbar
machine controls. Delete `routes/Dashboard.tsx` and the panel wrappers.
**Done when:** provider keys set/cleared from Settings; GitHub reconnect banner on
`409 reconnect_github`; clone from the launcher → progress via SSE → new project
tile; Logs window streams `machine_events`; no flat dashboard remains.

### 9.6 — Retire the PoC (Track A; independent)
Decision #8: delete `public/` and `server/`; move kept wallpaper(s) to
`web/public/`; strip PoC references from `package.json` scripts, deploy configs,
`RUNBOOK.md`/`DEPLOYMENT.md`, and `.github/workflows/ci.yml`.
**Done when:** repo builds and runs with no `server/` or `public/`; `grep -r` finds
no live references to the Express PoC; CI green.

### 9.6b — Playbook: extend the host acceptance gate (Track B; with 9.1/9.2)
No new packages or `group_vars` block (see "Deployment / host artifacts"): the
guest-agent change auto-rebakes via the SHA-keyed rootfs. The single deliverable
is to **extend the firecracker KVM acceptance suite** the `node_agent` role runs
as its pre-serve gate (`roles/node_agent/tasks/main.yml:273–289`) to boot a VM and
assert a `cwd`-scoped terminal lands in `/workspace/<repo>` and a
`projects.list`/`kv` round-trip works — so a host that bakes a broken guest agent
is caught before it serves.
**Done when:** the acceptance integration test covers cwd + projects.list + kv;
a clean `ansible-playbook site.yml` against a fresh Proxmox VM rebakes the rootfs
(new `ga<sha>`), passes the extended gate, and brings up a node-agent that serves
project-scoped sessions; the play is a no-op on re-run with unchanged source.

### 9.7 — Live acceptance on Proxmox (Track B; after 9.4–9.6b)
On the real stack with the GitHub App and a real microVM: clone a private repo →
Open editor on it (code-server opens the repo folder) → Open Claude Code in the
repo and run it (authenticated via the injected key) → Open a terminal in the
same repo → arrange/resize windows → reload the browser and confirm the layout +
live sessions restore → stop/start the machine and confirm clones + layout survive
(Phase-4 disk) → confirm no direct route to the guest (subdomain without cookie
401; port scan). Walk the master-plan Phase 9 checklist and tick the boxes in
`plans/proteos-poc-to-prod.md` (note Phase-4 gating on any stack without the disk).

### Sequencing

```
9.0 ──┬───────────────► 9.4 ──┐
9.1 ──┤                       ├──► 9.6 ──┐
9.2 ──┤                       │          ├──► 9.7 (live)
9.3 ──┤                 9.5 ──┘  9.6b ──┘
      └ (9.1/9.2/9.3 buildable in parallel once 9.0's shell exists;
         9.2's project set feeds 9.1's & 9.5's cwd/folder validation;
         9.6b extends the host acceptance gate alongside 9.1/9.2)
```

## Acceptance-criteria mapping (master-plan Phase 9 checklist)

| Criterion | Task(s) |
|---|---|
| Multiple windows (terminals, code-server, agents, logs, settings) open concurrently | 9.0 (manager) + 9.4 (terminal/editor/agent) + 9.5 (logs/settings) |
| Machine start/stop/status and the `machine_events` stream surfaced in the UI | 9.0 (taskbar controls/status) + 9.5 (Logs window from SSE) |
| Provider keys and GitHub connection managed from a settings UI | 9.5 (Settings window: Providers + GitHub tabs) |
| Window state (layout) persists via machine SQLite where appropriate | 9.3 (kv.* + `/api/machine/desktop`; theme/wallpaper in localStorage per decision #6) |
| PoC `public/` desktop is fully replaced | 9.6 (delete `public/` + `server/`); whole-phase rebuild |
| *(user throughline)* Clone → open agent/editor/terminal **in the repo folder** | 9.1 (cwd plumbing) + 9.2 (projects) + 9.4 (launcher) + 9.5 (clone) |

## Critical existing files to modify

- `controlplane/internal/guestctl/manager.go` — three new ops + relay helpers
- `controlplane/internal/httpapi/{gateway.go,agent.go}` — cwd parse/validate; opaque session; provider in path
- `controlplane/internal/httpapi/websession.go` + `internal/gateway/machineweb_token.go` — folder through mint→auth redirect
- `controlplane/internal/gateway/terminal.go` — `ProxyOpts.Cwd`, forwarded to guest
- `guestagent/api/guestwire.go` — handshake cwd/provider fields; new op consts/payloads
- `guestagent/internal/ctlchan/` and `internal/term/manager.go` — projects.list, kv.*, validated Dir
- `guestagent/internal/server/` — terminal/agent WS read cwd+provider, path guard
- `web/src/api/{client.ts,hooks.ts}`, `lib/terminalSocket.ts` — new endpoints, cwd/folder, opaque sessions
- `web/src/components/{MachineCard,ReposPanel,ProvidersPanel,GitHubStatus,Terminal,EditorPanel}.tsx` — recomposed into windows
- **Deleted:** `web/src/routes/Dashboard.tsx`, `public/`, `server/`
- `deploy/*`, `RUNBOOK.md`, `DEPLOYMENT.md`, `.github/workflows/ci.yml`, root `package.json` — drop PoC server
- `nodeagent/internal/driver/firecracker/*_test.go` — extend the KVM acceptance
  suite (cwd-scoped session + projects.list/kv) so the Ansible pre-serve gate covers Phase 9
- `deploy/ansible/roles/node_agent/tasks/main.yml` — **no new bake/var task** (guest agent
  auto-rebakes via the SHA-keyed rootfs); the only change is the extended acceptance gate above.
  Confirmed: `group_vars/all.yml` needs **no Phase 9 block**

## Verification

- **Unit/integration (any OS):** window-manager logic (z-order, min/max/restore,
  cascade, layout serialize/deserialize, **content-no-remount** on reorder);
  cwd/provider parse + validation matrix (good cwd, outside `/workspace`,
  nonexistent, not-listable); guest path guard; `projects.list` scan (git/non-git/
  deleted dirs); kv round-trip; folder-through-mint validation.
- **e2e (Mac, normal CI):** full convergence — login → machine → clone two repos →
  open editor/terminal/agent scoped to each → save & restore layout (reconnect to
  live PTYs) → settings (provider key set, GitHub reconnect) — against the dev
  driver + Phase 7 git harness; token never on disk (unchanged scan).
- **Host bake/gate (Proxmox):** 9.6b — a clean `ansible-playbook site.yml` against
  a fresh VM rebakes the SHA-keyed rootfs and passes the extended KVM acceptance
  suite (cwd-scoped session + projects.list/kv) before the node serves; re-run is a
  no-op on unchanged source. This is the "it's in the playbook" guarantee.
- **Live (Proxmox):** 9.7 — real App, real private repo, real agent run, reload-
  restore, stop/start durability, no-direct-route check.
- **CI:** no new KVM-gated work; e2e on standard runners; `tsc`/`vite build`/`vitest`.

## Non-goals / deferred

- **Tiling/snap layouts, virtual desktops, window grouping** — float-and-drag is
  the Phase 9 metaphor; richer layout management is a later product decision.
- **Light theme, wallpaper gallery, animated dock, sessions widget** — chosen
  scope is functional + single dark theme + one wallpaper (decision, 2026-06-16);
  the `localStorage` split (decision #6) leaves room to add a gallery later.
- **In-window git UI (branch/commit/PR), file tree outside code-server** — Phase 9
  ends at clone + open-in-folder; code-server provides the in-editor git UI.
- **Per-project resource isolation / multiple machines per user** — data model is
  1:1 user↔machine; multi-machine is post-Phase-11.
- **Idle-aware window/session teardown** — Phase 11 (idle detection must count
  agent activity); Phase 9 keeps sessions alive as long as windows are open.
- **Rate limiting on the new `/api/projects` & `/api/machine/desktop` routes** —
  Phase 10, with the rest of `/api/*` and `/gw/*`.
- **Cross-browser layout sync conflict resolution** — last-write-wins on the
  single `desktop.layout` key is acceptable for 1:1 machines; multi-tab merge is
  out of scope.
