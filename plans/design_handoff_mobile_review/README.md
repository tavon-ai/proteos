# Handoff: ProteOS Mobile — Purpose-Built PR Review + Machines (Option 1c)

## Overview
A **purpose-built mobile web UI** for ProteOS — not the desktop squeezed onto a phone. It exists for one dominant flow: a coding agent runs on a machine (kicked off from Telegram), and the user gets a Telegram link that **deep-links straight to the resulting pull request on their phone**. They skim the changed files, read the diff, and **Approve & merge** — the whole loop stays on the phone. A secondary **Machines** tab covers the other on-the-go need: see machine status and start/stop.

Everything the desktop does beyond this (draggable windows, terminals, editor, wallpaper, settings) is deliberately **absent** on mobile. Two screens, one primary action each.

## About the Design Files
The files in this bundle are **design references created in HTML** — a prototype of the intended look and behavior, **not production code to copy directly**. ProteOS's web client is **React 18 + TypeScript + Vite** with a REST + EventSource/SSE API (`web/src/api/client.ts`, `web/src/api/hooks.ts`). Recreate this design in that codebase — most cleanly as a **separate mobile route/entry** (or a viewport-gated shell) that reuses the existing API client, hooks, and CSS tokens. Do **not** attempt to make the desktop window-manager responsive; build these two dedicated screens.

Open `ProteOS_Redesign_reference.html` and scroll to the block labeled **1c**. **Tap the bottom tabs (Machines ↔ Review)** to move between the two screens. The phone bezel in the prototype is presentational — build a normal mobile web app, not the bezel.

## Fidelity
**High-fidelity.** All colors, sizes, spacing, and states are final and exact. Reuse the existing ProteOS tokens (GitHub-dark). Tap targets are ≥44px by design — preserve that.

---

## Screens / Views

### Global shell
- Background `#0d1117`, text `#e6edf3`, system font stack. Column layout: content area (`flex:1; min-height:0`) above a fixed **bottom tab bar**. Respect `env(safe-area-inset-bottom)` (the tab bar reserves ~22px bottom padding for the home indicator).
- Design width reference: 390px (iPhone). Everything is fluid — no fixed inner widths besides the tab items.

### Screen 1: Review (default / deep-link target)
**Purpose:** Land on a specific PR, review it, merge it.

**Layout (top → bottom):** fixed header → scrollable body (stats + files + diff) → sticky action bar.

**Header** — `padding:6px 18px 14px; border-bottom:1px solid #21262d`:
- **Top row** (`display:flex; align-items:center; justify-content:space-between; margin-bottom:12px`):
  - Back chevron, 24px stroke `#8b949e` (returns to Telegram / previous — or Machines).
  - Machine chip: `font-size:12.5px; color:#8b949e; display:inline-flex; gap:6px;` with a 7px running dot `#3fb950` (`box-shadow:0 0 7px #3fb950`) — shows which machine the branch is on.
  - Avatar: 29px circle, `linear-gradient(140deg,#db61a2,#a371f7)` (placeholder for `avatar_url`).
- **Status row** (`margin-bottom:6px; display:flex; align-items:center; gap:8px`):
  - "OPEN PR" chip: `height:22px; padding:0 8px; border-radius:6px; background:rgba(63,185,80,0.14); border:1px solid rgba(63,185,80,0.4); color:#3fb950; font-size:11px; font-weight:700;` with a 12px branch icon. (Merged → purple `#a371f7`; Closed → red `#f85149`; Draft → grey `#8b949e`.)
  - PR number, `font-size:12px; color:#8b949e` (e.g. "#284").
- **Title:** `font-size:17px; font-weight:700; line-height:1.35; letter-spacing:-0.01em` (e.g. "Node-agent reliability hardening").
- **Repo/branch:** mono `font-size:11.5px; color:#8b949e; margin-top:6px` (e.g. "tavon-ai/proteos · feat/tav-38-node-agent-reliability").

**Body** — scrollable (`flex:1; overflow:auto; min-height:0`):
- **Stat strip** — `padding:12px 18px; border-bottom:1px solid #21262d; display:flex; align-items:center; gap:14px; font-size:12.5px`: "N files" `#8b949e`; "+128" `#3fb950 / 600`; "−34" `#f85149 / 600`; right-aligned checks summary (`margin-left:auto`) with a 14px check icon `#3fb950` + "2 checks passed" `#8b949e`.
- **File rows** — each `padding:11px 18px; display:flex; align-items:center; gap:10px; cursor:pointer`:
  - Status letter: mono, 16px wide, centered, `font-weight:700`, colored — Modified `M` `#9a6700`, Added `A` `#1a7f37`, Deleted `D` `#f85149`, Renamed `R` `#8250df`.
  - Path: mono `font-size:12.5px`, ellipsized (`overflow:hidden; text-overflow:ellipsis; white-space:nowrap; flex:1`).
  - Per-file counts: "+61" `#3fb950`, "−12" `#f85149`, each `font-size:11px`.
  - **Selected row:** `background:rgba(47,129,247,0.07); border-left:2px solid #2f81f7`. **Hover:** `background:rgba(255,255,255,0.03)`. Last row: `border-bottom:1px solid #21262d`.
- **Diff view** (for the selected file) — mono `font-size:11.5px; line-height:1.7`, each line `padding:1px 18px`:
  - Hunk header: `color:#6b7480` (e.g. "@@ orphan reaper: SIGKILL after grace @@").
  - Context line: `color:#8b949e`.
  - Added line: `background:rgba(63,185,80,0.13); color:#8ee0a1`, prefixed `+`.
  - Removed line: `background:rgba(248,81,73,0.12); color:#f0a5a0`, prefixed `−`.

**Sticky action bar** — `padding:12px 18px; border-top:1px solid #21262d; background:rgba(13,17,23,0.9); backdrop-filter:blur(8px); display:flex; gap:10px`:
- **Comment button:** 46×46, `border-radius:12px; background:rgba(255,255,255,0.05); border:1px solid #30363d; color:#8b949e`, chat-bubble icon 20px. Opens a comment sheet.
- **Primary — "Approve & merge":** `flex:1; height:46px; border-radius:12px; background:#238636; border:1px solid #2ea043; color:#fff; font-size:15px; font-weight:700; display:flex; align-items:center; justify-content:center; gap:8px`, 18px check icon. (`#238636`/`#2ea043` is GitHub's established merge-green; if you prefer to stay strictly on the app palette, `--accent`-green `#3fb950` is acceptable.) This is the one thumb-reachable primary action.

### Screen 2: Machines
**Purpose:** Glance at machine status; start/stop.

**Layout:** header → scrollable card list → (shared) bottom tab bar.

**Header** — `padding:6px 18px 14px; border-bottom:1px solid #21262d; display:flex; align-items:center; justify-content:space-between`:
- Title "Machines" `font-size:20px; font-weight:700`.
- "＋ New" button: `height:32px; padding:0 13px; border-radius:9px; background:#2f81f7; color:#fff; font-size:13.5px; font-weight:600`.

**Machine cards** — list `padding:12px 16px; display:flex; flex-direction:column; gap:10px`. Each card `padding:15px; border-radius:13px; background:#161b22; border:1px solid #30363d; display:flex; align-items:center; gap:13px`:
- **Status dot** 9px, `flex:0 0 auto` — running `#3fb950` (`box-shadow:0 0 8px #3fb950`), stopped `#8b949e`, starting/transitional `#d29922`. Starting cards get `border-color:rgba(210,153,34,0.4)`.
- **Text block** (`flex:1; min-width:0`): name `font-size:15px; font-weight:700`; meta `font-size:11.5px; color:#8b949e` (e.g. "Running · 2 vCPU · 2 GiB · 10 GiB"). For a starting machine, meta reads "Starting…" in `#d29922`.
- **Trailing control** 40×40, `border-radius:10px`:
  - Running → **Stop**: filled square icon 16px, `background:rgba(255,255,255,0.05); border:1px solid #30363d; color:#8b949e`.
  - Stopped → **Start**: play triangle icon, `background:rgba(63,185,80,0.14); border:1px solid rgba(63,185,80,0.4); color:#3fb950`.
  - Starting/stopping → 16px spinner (`border:2px solid #30363d; border-top-color:#d29922; border-radius:50%; animation: spin 0.8s linear infinite`) instead of a button.

### Shared: Bottom Tab Bar
- Container: `padding:9px 8px 22px; border-top:1px solid #21262d; background:rgba(13,17,23,0.95); display:flex; justify-content:space-around; align-items:flex-start`. The `22px` bottom padding covers the home indicator (use `env(safe-area-inset-bottom)` in production).
- Three tabs, each `width:88px; display:flex; flex-direction:column; align-items:center; gap:4px`; icon 23px stroke 1.8; label `font-size:10.5px; font-weight:600`.
  - **Machines** — monitor icon. **Review** — connected-nodes icon. **Activity** — pulse icon (placeholder/next).
- **Active** color `#e6edf3`; **idle** color `#6b7480` (applied to both icon stroke and label). Default active tab on a deep-link is **Review**.

---

## Interactions & Behavior
- **Deep link (primary entry):** a Telegram link opens the app directly on the **Review** screen for a specific PR — e.g. route `/m/:machineId/pr/:number` (or `/review?machine=…&pr=…`). No intermediate navigation. The back chevron returns to the browser/Telegram (or to Machines if opened in-app).
- **File selection:** tapping a file row selects it (blue left-border + tint) and shows that file's diff below. Default-select the first file.
- **Approve & merge:** taps → confirmation → calls the merge endpoint; show an in-flight state on the button (spinner + "Merging…") and a success/failure result. On success, the OPEN PR chip flips to a purple "MERGED" state.
- **Comment:** opens a bottom sheet with a textarea + submit (posts a PR comment).
- **Tab switching:** Machines ↔ Review ↔ Activity swaps the content area; preserve scroll position per tab.
- **Machine start/stop:** the trailing control calls the existing machine lifecycle mutations; the card reflects live state from the SSE feed (dot + meta update in place; button becomes a spinner during transitions). "＋ New" opens the create-machine flow (can reuse the existing template picker, restyled full-screen for mobile).
- **No horizontal scrolling** anywhere; diff lines wrap or scroll only within the diff block if you choose (the mock keeps them on one line — a horizontal scroll region for the diff is acceptable).

## State Management
- **Current tab** (`'machines' | 'review' | 'activity'`) — local.
- **Review context:** `{ machineId, prNumber }` from the route; selected file index; merge-in-flight boolean; result state.
- **Machines:** live list from the existing `useMachines()` + SSE `useMachineEvents()`; lifecycle via the existing `useMachineMutations()` (`start`, `stop`).
- **PR + diff data:** fetch the PR summary, file list, and per-file diff. **Reuse the existing diff-fetch/parse logic** in `web/src/windows/ChangesWindow.tsx` (it already renders worktree diffs and status codes A/M/D/R and opens PRs) — the mobile Review screen is a phone-native presentation of that same data.
- Auth reuses the existing `/api/me` session gate.

## Design Tokens
Reuse ProteOS tokens (`web/src/styles.css :root`): `--bg:#0d1117`, `--panel:#161b22`, `--border:#30363d`, `--text:#e6edf3`, `--muted:#8b949e`, `--accent:#2f81f7`, `--error:#f85149`.

Additional exact values used here:
- Greens: status/running `#3fb950`; diff-added bg `rgba(63,185,80,0.13)`, added text `#8ee0a1`; merge primary `#238636` / border `#2ea043`.
- Reds: `#f85149`; diff-removed bg `rgba(248,81,73,0.12)`, removed text `#f0a5a0`; deleted-file letter same red.
- Status-letter colors: M `#9a6700`, A `#1a7f37`, D `#f85149`, R `#8250df`.
- Amber (transitional) `#d29922`. Purple (merged) `#a371f7`. Pink (avatar grad start) `#db61a2`.
- Surfaces: card `#161b22`; hairlines `#21262d`; idle-tab text `#6b7480`.

Radii: cards **13**, buttons/controls **10–12**, chips **6**, tab-bar none. Tap targets: primary **46**, controls **40–46** (never below 44 for interactive).

Typography: system stack; mono `ui-monospace, SFMono-Regular, Menlo, Consolas, monospace`. Sizes: PR title 17/700, machines title 20/700, machine name 15/700, body/meta 11.5–13, diff 11.5 mono, tab label 10.5/600.

## Assets
- All icons are inline stroke SVGs (back chevron, branch/nodes, check, chat bubble, monitor, pulse, play, stop/square, spinner). Use the codebase's icon set if one exists.
- Avatar: existing `me.user.avatar_url`.
- No images; no device bezel in production (the prototype's bezel is illustration only).

## Files
**In this bundle:**
- `README.md` — this document.
- `ProteOS_Redesign_reference.html` — the interactive prototype (block **1c**; tap the tabs).

**In the real repo (`web/src/`):**
- New mobile entry — e.g. `web/src/mobile/` (`MobileApp.tsx`, `ReviewScreen.tsx`, `MachinesScreen.tsx`, `TabBar.tsx`), reached via a viewport-gated route or a dedicated mobile build/route. Keep it separate from `desktop/`.
- Reuse `api/client.ts` + `api/hooks.ts` (machines, events, mutations, `me`).
- Reuse diff data + PR logic from `windows/ChangesWindow.tsx`.
- Reuse tokens from `styles.css` (add a small mobile stylesheet or CSS-module for the screens above).
- Shares the nav/tab model with the desktop **Left Rail (Option 1b)** handoff — build the token/API layer once.
