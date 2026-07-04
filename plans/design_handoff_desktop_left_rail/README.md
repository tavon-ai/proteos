# Handoff: ProteOS Desktop — Left Rail + Slim Context Bar (Option 1b)

## Overview
This redesigns the ProteOS desktop shell's navigation. Today a single 44px top bar (`Taskbar.tsx`) crams the brand, machine lifecycle controls (Stop/Destroy), five plain-text app launchers (Projects, Terminal, Open app…, Settings, Activity), a clock, and the account menu into one row — so nothing reads as a real control and destructive actions sit one mis-click away.

The new model:
- A **persistent, labeled left icon rail** becomes the primary navigation (icon + word, always visible, active section marked).
- The top bar shrinks to a **slim context bar**: machine pill + repo/branch breadcrumb + search + clock.
- **Stop / Destroy leave the always-visible chrome** and live inside the machine pill's menu (Destroy stays guarded).
- New windows spawn on a **consistent 24px margin + cascade offset** so they never appear flush to an edge or fully hidden behind another window.

This rail model maps 1:1 to the mobile bottom tab bar (see the mobile handoff), so the two surfaces read as one product.

## About the Design Files
The files in this bundle are **design references created in HTML** — a prototype showing the intended look and behavior, **not production code to copy directly**. The ProteOS web client is a **React 18 + TypeScript + Vite** SPA (`react-rnd` for window drag/resize, TanStack Query for data, an EventSource/SSE feed for live machine state). The task is to **recreate this design inside that existing codebase using its established patterns** — the same CSS-variable tokens in `web/src/styles.css`, the same window-manager and openers, the same machine store — not to ship the HTML.

Open `ProteOS_Redesign_reference.html` in a browser to interact with the prototype. Scroll to the block labeled **1b** (the desktop). It also contains 1a (an alternative not chosen) and 1c (mobile) for context.

## Fidelity
**High-fidelity.** Colors, typography, spacing, radii, and states below are final and exact. Recreate pixel-perfectly using the existing tokens. Every color used is already (or should become) a token in `styles.css`.

---

## Screens / Views

### The Desktop Shell (single view)
**Purpose:** The product shell — a project-centric, multi-window desktop over the active machine.

**Overall layout:** Replace today's vertical stack (`.taskbar` on top → surface → dock) with a **horizontal split**:

```
┌──────┬─────────────────────────────────────────────┐
│      │  Context bar (44px)                          │
│ Rail ├─────────────────────────────────────────────┤
│ 76px │  Window surface (flex:1, position:relative)  │
│      │                                              │
│      ├─────────────────────────────────────────────┤
│      │  Dock (44px, unchanged)                      │
└──────┴─────────────────────────────────────────────┘
```

- Root `.desktop`: `position:fixed; inset:0; display:flex; flex-direction:row;` (was `column`). Wallpaper background is unchanged (`background-size:cover` of the user's wallpaper, with the existing radial-gradient overlay).
- The rail is the first flex child (full height). The **main column** (`flex:1; display:flex; flex-direction:column; min-width:0`) holds the context bar, the window surface, and the dock.

---

### Component: Left Rail
- **Container:** `width:76px; flex:0 0 76px;` full height. `display:flex; flex-direction:column; align-items:center; gap:6px; padding:12px 0;`. Background `rgba(13,17,23,0.86)` + `backdrop-filter:blur(10px)`; `border-right:1px solid #30363d`; `z-index:20`.
- **Brand mark (top):** 34×34, `border-radius:9px`, `background:linear-gradient(150deg,#2f81f7,#0e7490)`, `margin-bottom:10px`. Contains a 19px white wave glyph (2 stacked sine strokes, `stroke-width:1.9`, `stroke-linecap:round`) — the Proteus/ocean mark.
- **Rail item (nav button):** 60×56, `display:flex; flex-direction:column; align-items:center; justify-content:center; gap:4px; border-radius:11px; cursor:pointer;`.
  - Icon: 20px, stroke-based, `stroke-width:1.7`, `fill:none`, color = current text color.
  - Label: `font-size:10.5px; font-weight:600;` directly under the icon.
  - **Idle:** `background:transparent; color:#8b949e`.
  - **Active:** `background:rgba(219,97,162,0.14); color:#e6edf3;` + a 3px active-accent bar pinned to the rail's left edge: an absolutely-positioned span `left:-3px; top:12px; bottom:12px; width:3px; border-radius:3px; background:<section color>`. The tint and bar color are the section's dock-kind color (see Design Tokens → Section colors). In the mock, Projects is active → pink `#db61a2`.
  - **Hover (idle items):** `background:rgba(255,255,255,0.05); color:#e6edf3`.
- **Rail items, in order (top group):**
  1. **Projects** — folder icon. Opens/focuses the Projects launcher for the active machine (`openProjects`). Dock-kind color `#db61a2` (pink).
  2. **Terminal** — terminal icon (rounded rect + `>` + line). Opens a home terminal on the active machine (`openHomeTerminal`). Color `#3fb950` (green).
  3. **Agents** — connected-nodes icon (3 circles + links). Launches/focuses a coding-agent terminal. Color `#d29922` (amber).
  4. **Activity** — pulse/waveform icon. Opens the global Activity/logs window (`openLogs`). Color `#8b949e`.
- **Rail items (bottom group):** pushed down with `margin-top:auto` on a wrapper (`display:flex; flex-direction:column; align-items:center; gap:6px`).
  5. **Settings** — sliders icon, 60×52 (slightly shorter). Opens the global Settings window (`openSettings`). Color `#a371f7` (purple).
  6. **Avatar** — 30×30 circle, `background:linear-gradient(140deg,#db61a2,#a371f7)` (placeholder for `me.user.avatar_url`). Opens the account menu (Sign out lives here now).
- **Disabled state:** Projects/Terminal/Agents require a selected running machine — when none, render at `opacity:0.4; cursor:not-allowed` and no-op (mirrors today's `disabled={!selectedId}`).

Because labels are always visible, **no tooltips are required**.

### Component: Context Bar (slim top bar)
- **Container:** `height:44px; display:flex; align-items:center; gap:12px; padding:0 14px;` Background `rgba(13,17,23,0.70)` + `backdrop-filter:blur(10px)`; `border-bottom:1px solid #30363d`; `z-index:15`.
- **Machine pill (left):** the consolidated machine control (replaces today's `MachineSwitcher` badge + Start/Stop/Destroy buttons). `height:30px; padding:0 10px; border-radius:8px; display:flex; align-items:center; gap:8px; white-space:nowrap; cursor:pointer;`.
  - Running: `background:rgba(63,185,80,0.12); border:1px solid rgba(63,185,80,0.4)`. Status dot 7px `#3fb950` with `box-shadow:0 0 8px #3fb950`. Name `font-size:13px; font-weight:600`. Trailing chevron 12px `#8b949e`.
  - The pill's background/border follow machine state using the existing badge palette (running green, stopped grey `#8b949e`, error red `#f85149`, transitional blue `#2f81f7`).
  - **Click → machine menu** (reuse the existing `.machine-menu` dropdown): list of machines to switch, a separator, then **Details**, **Rename**, **+ New machine**, and the **lifecycle actions** — Start/Stop for the active machine and a **guarded Destroy** (keep the existing `window.confirm` "Destroy this machine? …" gate). This is the key safety change: Destroy is no longer a red button in the chrome.
- **Breadcrumb (after pill):** `display:flex; align-items:center; gap:7px; font-size:13px;`. Repo name in mono `#c9d1d9`; a `/` separator `#4a525c`; the branch as a chip: mono `font-size:11.5px; color:#8b949e; background:#21262d; border-radius:5px; padding:2px 7px`. Reflects the active project/branch (from the focused project window; hide when none).
- **Search button (`margin-left:auto`):** `height:30px; padding:0 11px; border-radius:8px; background:rgba(255,255,255,0.05); border:1px solid #30363d; color:#8b949e; display:flex; align-items:center; gap:9px;`. Search icon 14px; label "Search…" 12.5px; a `⌘K` kbd hint (mono 10.5px, `border:1px solid #30363d; border-radius:4px; padding:1px 5px`). Opens a lightweight command palette (see Interactions — optional but recommended; can ship as plain global search first).
- **Clock (far right):** mono `font-size:13px; color:#8b949e; font-variant-numeric:tabular-nums`. Same 30s-interval `useClock` as today.

### Component: Window (chrome) — mostly unchanged, spacing fixed
Keep the existing `react-rnd` window from `Window.tsx`. Visual spec (already in `styles.css`, restated for exactness):
- `.window-frame`: `background:#161b22; border:1px solid #30363d; border-radius:10px; overflow:hidden; box-shadow:0 12px 40px rgba(0,0,0,0.5)`.
- `.window-header`: `height:34px; background:#1c2230; border-bottom:1px solid #30363d; padding:0 8px 0 13px; cursor:move`. Title 13px/600, ellipsized. A per-window kind dot (8px) before the title, colored by dock-kind. Controls (minimize `–`, maximize `▢`/`❐`, close `✕`) 24×20 each, `color:#8b949e`; close hover `background:#f85149; color:#fff`.
- `MIN_WIDTH:280, MIN_HEIGHT:180` unchanged.

---

## Interactions & Behavior
- **Rail navigation:** clicking a rail item opens (or focuses, if already open) that window kind for the active machine, using the existing `openers.ts` helpers. No route change — it's window-manager state.
- **Active-section indicator:** the rail highlights the section matching the **currently focused window's kind** (`wm` tracks `topZ`/focus). When the focused window is a terminal, Terminal is active; a project launcher → Projects; agent terminal → Agents; logs → Activity; settings → Settings. When nothing relevant is focused, no item is active.
- **Machine menu:** click the pill to open; backdrop click closes (reuse `.machine-menu-backdrop`). All existing mutations (`start`, `stop`, `destroy`, `rename`) and the create/details modals move in here unchanged.
- **Window spawn (the sizing/margins fix):** new windows are placed by a cascade generator instead of ad-hoc positions. Base at `x:24, y:24` (inside the surface, which already sits below the 44px context bar). Each subsequent open offsets `+28px` on both axes. Clamp so the whole window stays inside the surface: if `x + width > surfaceW - 24`, reset `x` to 24 (and step the column); same for `y`. Default new-window size ~ `min(720, surfaceW-48) × min(460, surfaceH-48)`. This lives in `windowState.ts` (the geometry generator), **not** in per-caller code.
- **Maximize viewport:** `useViewport` must now subtract the rail horizontally and the context bar vertically: `width = innerWidth - 76`, `height = innerHeight - 44 - <dockHeight>`. Maximized windows fill the surface only (never cover the rail).
- **Command palette (optional, recommended):** `⌘K` / click Search opens a centered palette over the surface (`background:rgba(4,7,11,0.55)`, panel `#161b22`, `border:1px solid #3d4450`, `border-radius:14px`, `width:560px`, sectioned list: Actions — Clone a repo, Open terminal, Run coding agent — and Projects to clone/open). `esc` or backdrop click closes. If descoped for v1, the Search button can open the existing Projects launcher / a plain filter.
- **Transitions:** rail item background/color `transition:background .12s`. No large motion; keep it snappy.

## State Management
Almost entirely reuses existing state — this is a re-layout, not new data:
- `useSelectedMachine()` — active machine, machine list, `setSelectedId` (unchanged).
- `useWindowManager()` — windows, `focus/move/resize/minimize/toggleMaximize/close/hydrateMachine` (unchanged). Add a derived `focusedKind` selector (from the top-z window) to drive the rail's active state.
- `useMachineMutations()` — start/stop/destroy/rename (unchanged; now invoked from the machine menu).
- Layout persistence (`useLayoutSaver`/`useLayoutLoader`) — unchanged.
- New local UI state: machine-menu open boolean, command-palette open boolean.

## Design Tokens
Existing tokens in `web/src/styles.css :root` (use these; do not invent new hexes):
- `--bg:#0d1117` · `--panel:#161b22` · `--border:#30363d` · `--text:#e6edf3` · `--muted:#8b949e` · `--accent:#2f81f7` · `--error:#f85149`

Additional palette values already used across the app (promote to tokens if you like):
- Running green `#3fb950` · Amber `#d29922` · Purple `#a371f7` · Pink `#db61a2` · Header surface `#1c2230` · Tile surface `#12161d` · Branch-chip surface `#21262d` · Terminal surface `#0b0e14`

**Section (dock-kind) colors** — reuse the existing `.dock-kind-*` set for rail icons/active accents:
- projects `#db61a2` · terminal `#3fb950` · agent `#d29922` · editor `#2f81f7` · logs/activity `#8b949e` · settings `#a371f7`

Spacing: rail width **76**, rail item **60×56**, context bar **44**, window header **34**, window radius **10**, control radii **7–11**, window margin/cascade **24 / +28**.

Typography: system stack (`-apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif`); mono `ui-monospace, SFMono-Regular, Menlo, Consolas, monospace`. Sizes: rail label 10.5/600, context text 13, breadcrumb branch 11.5, window title 13/600, clock 13 tabular.

Chrome backgrounds: rail `rgba(13,17,23,0.86)` blur 10; context bar `rgba(13,17,23,0.70)` blur 10; dock `rgba(22,27,34,0.92)` (unchanged).

## Assets
- Brand wave glyph: inline SVG (2 sine strokes), no external asset.
- All icons are inline stroke SVGs (folder, terminal, nodes, pulse, sliders, chevron, search). Match to the codebase's existing icon set if one exists; otherwise inline these.
- User avatar: existing `me.user.avatar_url`.
- Wallpaper: existing user wallpaper system (`wallpaper.ts` / `WallpaperPanel`), unchanged.

## Files
**In this bundle:**
- `README.md` — this document.
- `ProteOS_Redesign_reference.html` — the interactive prototype (see block **1b**).

**In the real repo to change (`web/src/`):**
- `desktop/Taskbar.tsx` → split into a new `desktop/LeftRail.tsx` + `desktop/ContextBar.tsx`. The `MachineSwitcher` moves into the context bar (pill) with lifecycle actions folded into its menu. Delete the `taskbar-apps` text-button nav.
- `desktop/Desktop.tsx` (`DesktopShell`) → change `.desktop` to a row layout (rail + main column); update `useViewport` math (subtract rail width + context bar).
- `desktop/openers.ts` → unchanged; rail items call these.
- `desktop/windowState.ts` → add the 24px-margin + 28px-cascade geometry generator; clamp to surface.
- `desktop/Window.tsx` → unchanged (optionally add the kind dot in the header).
- `styles.css` → replace `.taskbar*` rules with `.rail*` and `.context-bar*`; keep all tokens and `.window-*`, `.dock-*`, `.machine-menu*` rules.
