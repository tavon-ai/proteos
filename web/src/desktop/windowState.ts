// windowState is the PURE, framework-free core of the desktop window manager
// (Phase 9 decision #2). The React context (WindowManager.tsx) is a thin shell
// over this reducer; keeping the logic here makes z-order, min/max/restore,
// cascade placement, and layout (de)serialization unit-testable without a DOM.
//
// The window array order is STABLE: windows are appended on open and removed on
// close, never reordered. Stacking is expressed purely through `zIndex` (a CSS
// concern), so React renders the same element order every time and a window's
// content subtree is never remounted by a focus/raise — the mount-once invariant
// the editor iframe and live terminals depend on.

type WindowKind =
  | 'terminal'
  | 'agent'
  | 'editor'
  | 'preview'
  | 'changes'
  | 'tasks'
  | 'logs'
  | 'applogs'
  | 'sessions'
  | 'settings'
  | 'projects'
  | 'placeholder';

type WindowMode = 'normal' | 'minimized' | 'maximized';

export interface Geometry {
  x: number;
  y: number;
  width: number;
  height: number;
}

export interface WindowState {
  id: string;
  kind: WindowKind;
  title: string;

  // The machine this window belongs to (multi-machine). Undefined ⇒ a global
  // window (Settings/Activity) shown regardless of the active machine. Windows
  // for the non-active machine stay mounted but hidden, so their terminals keep
  // their live PTYs across switches.
  machineId?: string;

  // Scoping (Phase 9): the project a window belongs to and the parameters its
  // content needs to (re)connect. `session` is the opaque, stable per-window id
  // a terminal/agent reconnects to; `cwd`/`folder` scope it to a repo folder.
  projectId?: string;
  session?: string;
  provider?: string;
  cwd?: string;
  folder?: string;
  // The in-machine application port a preview window proxies (PP3). Set only for
  // the 'preview' kind; the window mints a per-port web-session and frames it.
  port?: number;

  geometry: Geometry;
  zIndex: number;
  mode: WindowMode;
  // The geometry to return to when a maximized window is restored.
  restore?: Geometry;
}

export interface DesktopState {
  windows: WindowState[];
  topZ: number;
  cascade: number;
  // The window-surface size (the area between rail, context bar, and dock).
  // Fed by the shell via 'setSurface' so cascade placement can clamp windows
  // fully inside the surface. Undefined until the shell measures it.
  surface?: { width: number; height: number };
}

export const initialDesktop: DesktopState = { windows: [], topZ: 0, cascade: 0 };

// Default sizes per window kind. Editors and terminals want room; settings/logs
// are narrower utility windows.
const DEFAULT_SIZE: Record<WindowKind, { width: number; height: number }> = {
  terminal: { width: 720, height: 460 },
  agent: { width: 760, height: 520 },
  editor: { width: 1000, height: 680 },
  preview: { width: 900, height: 640 },
  changes: { width: 760, height: 560 },
  tasks: { width: 860, height: 580 },
  logs: { width: 560, height: 420 },
  applogs: { width: 760, height: 540 },
  sessions: { width: 820, height: 560 },
  settings: { width: 640, height: 540 },
  projects: { width: 600, height: 480 },
  placeholder: { width: 480, height: 320 },
};

const CASCADE_STEP = 28;
// Every window spawns at least this far from every surface edge (the design's
// consistent 24px margin), cascading +28/+28 per open.
const SURFACE_MARGIN = 24;
// Fallback wrap when the surface has not been measured yet.
const CASCADE_WRAP = 8;

// OpenSpec describes a window to open. An optional dedupeKey collapses repeat
// opens (e.g. the single Settings window, or an editor already on a folder) onto
// the existing window — focusing it instead of stacking a duplicate.
export interface OpenSpec {
  id: string;
  kind: WindowKind;
  title: string;
  machineId?: string;
  projectId?: string;
  session?: string;
  provider?: string;
  cwd?: string;
  folder?: string;
  port?: number;
  dedupeKey?: string;
  geometry?: Partial<Geometry>;
}

export type DesktopAction =
  | { type: 'open'; spec: OpenSpec }
  | { type: 'close'; id: string }
  | { type: 'focus'; id: string }
  | { type: 'move'; id: string; x: number; y: number }
  | { type: 'resize'; id: string; geometry: Geometry }
  | { type: 'minimize'; id: string }
  | { type: 'toggleMaximize'; id: string; viewport?: { width: number; height: number } }
  | { type: 'restore'; id: string }
  | { type: 'setSurface'; surface: { width: number; height: number } }
  | { type: 'hydrate'; windows: PersistedWindow[] }
  | { type: 'hydrateMachine'; machineId: string; windows: PersistedWindow[] };

// raise returns the next zIndex and bumps topZ.
function withTop(state: DesktopState): { z: number; topZ: number } {
  const z = state.topZ + 1;
  return { z, topZ: z };
}

// cascadeGeometry places the next window: base at the 24px surface margin,
// +28/+28 per subsequent open. With a measured surface it clamps the size so
// the window plus margins fits, and wraps each axis independently back to the
// margin when the next step would push the window within 24px of an edge — so
// a spawned window is never flush to an edge or fully off-surface.
function cascadeGeometry(
  cascade: number,
  size: { width: number; height: number },
  surface?: { width: number; height: number },
): { x: number; y: number; width: number; height: number; next: number } {
  if (!surface) {
    const i = cascade % CASCADE_WRAP;
    return {
      x: SURFACE_MARGIN + i * CASCADE_STEP,
      y: SURFACE_MARGIN + i * CASCADE_STEP,
      width: size.width,
      height: size.height,
      next: cascade + 1,
    };
  }
  const width = Math.max(1, Math.min(size.width, surface.width - 2 * SURFACE_MARGIN));
  const height = Math.max(1, Math.min(size.height, surface.height - 2 * SURFACE_MARGIN));
  // Steps that fit before x + width would cross surface.width - 24 (≥ 1).
  const stepsX = Math.max(
    1,
    Math.floor((surface.width - SURFACE_MARGIN - width - SURFACE_MARGIN) / CASCADE_STEP) + 1,
  );
  const stepsY = Math.max(
    1,
    Math.floor((surface.height - SURFACE_MARGIN - height - SURFACE_MARGIN) / CASCADE_STEP) + 1,
  );
  return {
    x: SURFACE_MARGIN + (cascade % stepsX) * CASCADE_STEP,
    y: SURFACE_MARGIN + (cascade % stepsY) * CASCADE_STEP,
    width,
    height,
    next: cascade + 1,
  };
}

// dedupeMatch finds an existing window matching spec's dedupeKey (kind + key).
function dedupeMatch(state: DesktopState, spec: OpenSpec): WindowState | undefined {
  if (!spec.dedupeKey) return undefined;
  return state.windows.find((w) => w.kind === spec.kind && dedupeKeyOf(w) === spec.dedupeKey);
}

// dedupeKeyOf reconstructs a window's dedupe identity from its fields. Editors
// dedupe by machine+folder and projects by machine (so each machine gets its own
// editor-per-folder and its own projects launcher); settings/activity are global
// singletons (one across all machines). The openers build matching dedupeKeys.
function dedupeKeyOf(w: WindowState): string | undefined {
  switch (w.kind) {
    case 'editor':
      return `${w.machineId ?? ''}|${w.folder ?? w.projectId ?? ''}`;
    case 'preview':
      return `${w.machineId ?? ''}|${w.port ?? ''}`;
    case 'changes':
      return `${w.machineId ?? ''}|${w.projectId ?? ''}`;
    case 'tasks':
      return `${w.machineId ?? ''}|${w.projectId ?? ''}`;
    case 'projects':
      return `projects|${w.machineId ?? ''}`;
    case 'settings':
    case 'logs':
    case 'applogs':
    case 'sessions':
      return w.kind;
    default:
      return undefined;
  }
}

export function desktopReducer(state: DesktopState, action: DesktopAction): DesktopState {
  switch (action.type) {
    case 'open': {
      const existing = dedupeMatch(state, action.spec);
      if (existing) {
        // Collapse onto the existing window: focus + un-minimize it.
        return desktopReducer(state, { type: 'focus', id: existing.id });
      }
      const size = DEFAULT_SIZE[action.spec.kind];
      const place = cascadeGeometry(state.cascade, size, state.surface);
      const { z, topZ } = withTop(state);
      const geometry: Geometry = {
        x: action.spec.geometry?.x ?? place.x,
        y: action.spec.geometry?.y ?? place.y,
        width: action.spec.geometry?.width ?? place.width,
        height: action.spec.geometry?.height ?? place.height,
      };
      const win: WindowState = {
        id: action.spec.id,
        kind: action.spec.kind,
        title: action.spec.title,
        machineId: action.spec.machineId,
        projectId: action.spec.projectId,
        session: action.spec.session,
        provider: action.spec.provider,
        cwd: action.spec.cwd,
        folder: action.spec.folder,
        port: action.spec.port,
        geometry,
        zIndex: z,
        mode: 'normal',
      };
      return { ...state, windows: [...state.windows, win], topZ, cascade: place.next };
    }

    case 'close':
      return { ...state, windows: state.windows.filter((w) => w.id !== action.id) };

    case 'focus': {
      const win = state.windows.find((w) => w.id === action.id);
      if (!win) return state;
      // Already on top and not minimized ⇒ nothing changes (avoid a needless
      // re-render and zIndex churn).
      if (win.zIndex === state.topZ && win.mode !== 'minimized') return state;
      const { z, topZ } = withTop(state);
      return {
        ...state,
        topZ,
        windows: state.windows.map((w) =>
          w.id === action.id
            ? { ...w, zIndex: z, mode: w.mode === 'minimized' ? 'normal' : w.mode }
            : w,
        ),
      };
    }

    case 'move':
      return {
        ...state,
        windows: state.windows.map((w) =>
          w.id === action.id ? { ...w, geometry: { ...w.geometry, x: action.x, y: action.y } } : w,
        ),
      };

    case 'resize':
      return {
        ...state,
        windows: state.windows.map((w) =>
          w.id === action.id ? { ...w, geometry: action.geometry } : w,
        ),
      };

    case 'minimize':
      return {
        ...state,
        windows: state.windows.map((w) => (w.id === action.id ? { ...w, mode: 'minimized' } : w)),
      };

    case 'restore':
      return {
        ...state,
        windows: state.windows.map((w) =>
          w.id === action.id
            ? { ...w, mode: 'normal', geometry: w.restore ?? w.geometry, restore: undefined }
            : w,
        ),
      };

    case 'toggleMaximize': {
      const win = state.windows.find((w) => w.id === action.id);
      if (!win) return state;
      const { z, topZ } = withTop(state);
      if (win.mode === 'maximized') {
        return {
          ...state,
          topZ,
          windows: state.windows.map((w) =>
            w.id === action.id
              ? {
                  ...w,
                  mode: 'normal',
                  zIndex: z,
                  geometry: w.restore ?? w.geometry,
                  restore: undefined,
                }
              : w,
          ),
        };
      }
      const vp = action.viewport;
      const maxedGeometry: Geometry = vp
        ? { x: 0, y: 0, width: vp.width, height: vp.height }
        : win.geometry;
      return {
        ...state,
        topZ,
        windows: state.windows.map((w) =>
          w.id === action.id
            ? { ...w, mode: 'maximized', zIndex: z, restore: w.geometry, geometry: maxedGeometry }
            : w,
        ),
      };
    }

    case 'setSurface':
      // Same size ⇒ same state reference (the shell reports on every resize).
      if (
        state.surface?.width === action.surface.width &&
        state.surface?.height === action.surface.height
      ) {
        return state;
      }
      return { ...state, surface: action.surface };

    case 'hydrate':
      return hydrate(state, action.windows);

    case 'hydrateMachine':
      return hydrateMachine(state, action.machineId, action.windows);

    default:
      return state;
  }
}

// focusedWindow returns the top-most visible window for the active machine:
// non-minimized, and either global (no machineId) or belonging to selectedId.
// The rail derives its active section from this window's kind, and the context
// bar its repo/branch breadcrumb. Undefined when nothing relevant is focused.
export function focusedWindow(
  windows: WindowState[],
  selectedId: string | null,
): WindowState | undefined {
  let top: WindowState | undefined;
  for (const w of windows) {
    if (w.mode === 'minimized') continue;
    if (w.machineId && w.machineId !== selectedId) continue;
    if (!top || w.zIndex > top.zIndex) top = w;
  }
  return top;
}

// --- Layout (de)serialization (Phase 9 decision #6) --------------------------

// PersistedWindow is the on-disk shape of a window in the saved layout. zIndex is
// not persisted — it is re-derived from array order on hydrate, so stacking is
// stable but compact. Maximized state collapses to its restore geometry so a
// reload reopens windows at a usable size.
export interface PersistedWindow {
  id: string;
  kind: WindowKind;
  title: string;
  machineId?: string;
  projectId?: string;
  session?: string;
  provider?: string;
  cwd?: string;
  folder?: string;
  port?: number;
  geometry: Geometry;
  mode?: WindowMode;
}

export interface PersistedLayout {
  version: 1;
  windows: PersistedWindow[];
}

// serializeLayout flattens the live desktop to its persisted form, dropping
// transient windows that should not be restored (the projects launcher and any
// placeholder are reopened by the shell, not the saved layout).
// machineId, when given, restricts the layout to that machine's windows — each
// machine's layout is persisted to its own guest SQLite, so a multi-machine
// desktop saves one machine's subset at a time.
export function serializeLayout(state: DesktopState, machineId?: string): PersistedLayout {
  const windows = state.windows
    .filter(
      (w) =>
        w.kind !== 'placeholder' &&
        w.kind !== 'projects' &&
        w.kind !== 'changes' &&
        w.kind !== 'tasks',
    )
    .filter((w) => machineId === undefined || w.machineId === machineId)
    .map<PersistedWindow>((w) => ({
      id: w.id,
      kind: w.kind,
      title: w.title,
      machineId: w.machineId,
      projectId: w.projectId,
      session: w.session,
      provider: w.provider,
      cwd: w.cwd,
      folder: w.folder,
      port: w.port,
      // A maximized window persists at its restore geometry, not full-screen.
      geometry: w.mode === 'maximized' ? (w.restore ?? w.geometry) : w.geometry,
      mode: w.mode === 'minimized' ? 'minimized' : 'normal',
    }));
  return { version: 1, windows };
}

// hydrate rebuilds desktop state from a persisted layout, assigning zIndex by
// array order (last = top) so the saved stacking is preserved deterministically.
// Transient windows already on the desktop (the Projects launcher, any
// placeholder) are NOT persisted but must survive a hydrate — they are the
// always-available shell surface — so they are kept beneath the restored
// windows. A persisted window whose id collides with a kept transient one wins
// (the restored window replaces it).
function hydrate(current: DesktopState, windows: PersistedWindow[]): DesktopState {
  const restoredIds = new Set(windows.map((w) => w.id));
  const kept = current.windows.filter(
    (w) => (w.kind === 'projects' || w.kind === 'placeholder') && !restoredIds.has(w.id),
  );
  const restored = windows.map<WindowState>((w, i) => ({
    id: w.id,
    kind: w.kind,
    title: w.title,
    machineId: w.machineId,
    projectId: w.projectId,
    session: w.session,
    provider: w.provider,
    cwd: w.cwd,
    folder: w.folder,
    port: w.port,
    geometry: w.geometry,
    zIndex: kept.length + i + 1,
    mode: w.mode === 'minimized' ? 'minimized' : 'normal',
  }));
  // Re-number kept transients beneath the restored stack.
  const keptRenum = kept.map((w, i) => ({ ...w, zIndex: i + 1 }));
  const all = [...keptRenum, ...restored];
  return { windows: all, topZ: all.length, cascade: all.length, surface: current.surface };
}

// hydrateMachine restores one machine's persisted windows WITHOUT disturbing any
// other machine's live windows (the multi-machine desktop loads each machine's
// layout the first time it is selected). Existing windows for this machine are
// replaced by the restored set; windows for every other machine (and global
// windows) are kept exactly as-is. zIndex is renumbered by final array order.
function hydrateMachine(
  current: DesktopState,
  machineId: string,
  windows: PersistedWindow[],
): DesktopState {
  const others = current.windows.filter((w) => w.machineId !== machineId);
  const restored = windows.map<WindowState>((w) => ({
    id: w.id,
    kind: w.kind,
    title: w.title,
    machineId,
    projectId: w.projectId,
    session: w.session,
    provider: w.provider,
    cwd: w.cwd,
    folder: w.folder,
    port: w.port,
    geometry: w.geometry,
    zIndex: 0, // renumbered below
    mode: w.mode === 'minimized' ? 'minimized' : 'normal',
  }));
  const all = [...others, ...restored].map((w, i) => ({ ...w, zIndex: i + 1 }));
  return { windows: all, topZ: all.length, cascade: all.length, surface: current.surface };
}

// parseLayout safely parses a stored layout JSON string, returning null on any
// malformed input (a corrupt layout must never crash the desktop boot).
export function parseLayout(raw: unknown): PersistedLayout | null {
  if (raw == null) return null;
  let obj: unknown = raw;
  if (typeof raw === 'string') {
    try {
      obj = JSON.parse(raw);
    } catch {
      return null;
    }
  }
  if (typeof obj !== 'object' || obj === null) return null;
  const layout = obj as Partial<PersistedLayout>;
  if (layout.version !== 1 || !Array.isArray(layout.windows)) return null;
  // Keep only structurally valid window entries.
  const windows = layout.windows.filter(
    (w): w is PersistedWindow =>
      !!w &&
      typeof w.id === 'string' &&
      typeof w.kind === 'string' &&
      typeof w.title === 'string' &&
      !!w.geometry &&
      typeof w.geometry.x === 'number' &&
      typeof w.geometry.y === 'number' &&
      typeof w.geometry.width === 'number' &&
      typeof w.geometry.height === 'number',
  );
  return { version: 1, windows };
}
