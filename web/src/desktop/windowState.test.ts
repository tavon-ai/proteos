import { describe, expect, it } from 'vitest';
import {
  desktopReducer,
  initialDesktop,
  serializeLayout,
  parseLayout,
  type DesktopState,
  type OpenSpec,
} from './windowState';

function open(state: DesktopState, spec: Partial<OpenSpec> & { id: string }): DesktopState {
  return desktopReducer(state, {
    type: 'open',
    spec: { kind: 'placeholder', title: spec.id, ...spec },
  });
}

describe('desktopReducer', () => {
  it('opens windows with ascending z-order and cascade placement', () => {
    let s = initialDesktop;
    s = open(s, { id: 'a' });
    s = open(s, { id: 'b' });
    s = open(s, { id: 'c' });
    expect(s.windows.map((w) => w.id)).toEqual(['a', 'b', 'c']);
    // z-order ascends with open order; c is on top.
    expect(s.windows[0].zIndex).toBeLessThan(s.windows[2].zIndex);
    // Cascade offsets each successive window.
    expect(s.windows[1].geometry.x).toBeGreaterThan(s.windows[0].geometry.x);
    expect(s.windows[1].geometry.y).toBeGreaterThan(s.windows[0].geometry.y);
  });

  it('cascades from the 24px surface margin in +28 steps', () => {
    let s = desktopReducer(initialDesktop, {
      type: 'setSurface',
      surface: { width: 1200, height: 800 },
    });
    s = open(s, { id: 'a' });
    s = open(s, { id: 'b' });
    expect(s.windows[0].geometry.x).toBe(24);
    expect(s.windows[0].geometry.y).toBe(24);
    expect(s.windows[1].geometry.x).toBe(52);
    expect(s.windows[1].geometry.y).toBe(52);
  });

  it('clamps a window larger than the surface to fit inside the margins', () => {
    let s = desktopReducer(initialDesktop, {
      type: 'setSurface',
      surface: { width: 500, height: 400 },
    });
    s = desktopReducer(s, {
      type: 'open',
      spec: { id: 'e', kind: 'editor', title: 'E' }, // default 1000×680
    });
    expect(s.windows[0].geometry).toEqual({ x: 24, y: 24, width: 452, height: 352 });
  });

  it('wraps an axis back to the margin instead of crossing within 24px of an edge', () => {
    // Surface fits the 480-wide placeholder with zero horizontal slack and four
    // 28px steps of vertical slack: x stays pinned at 24, y wraps after 5 opens.
    let s = desktopReducer(initialDesktop, {
      type: 'setSurface',
      surface: { width: 528, height: 500 },
    });
    for (let i = 0; i < 6; i++) s = open(s, { id: `w${i}` });
    expect(s.windows.map((w) => w.geometry.x)).toEqual([24, 24, 24, 24, 24, 24]);
    expect(s.windows.map((w) => w.geometry.y)).toEqual([24, 52, 80, 108, 136, 24]);
    // Every window sits fully inside the surface with the 24px margin.
    for (const w of s.windows) {
      expect(w.geometry.y + w.geometry.height).toBeLessThanOrEqual(500 - 24);
    }
  });

  it('focus raises z-order without reordering the windows array', () => {
    let s = initialDesktop;
    s = open(s, { id: 'a' });
    s = open(s, { id: 'b' });
    const order = s.windows.map((w) => w.id);
    s = desktopReducer(s, { type: 'focus', id: 'a' });
    // Array order is STABLE (mount-once invariant) — only zIndex changes.
    expect(s.windows.map((w) => w.id)).toEqual(order);
    const a = s.windows.find((w) => w.id === 'a')!;
    const b = s.windows.find((w) => w.id === 'b')!;
    expect(a.zIndex).toBeGreaterThan(b.zIndex);
  });

  it('focusing the already-top window is a no-op (same reference)', () => {
    let s = initialDesktop;
    s = open(s, { id: 'a' });
    s = open(s, { id: 'b' });
    const before = s;
    s = desktopReducer(s, { type: 'focus', id: 'b' });
    expect(s).toBe(before);
  });

  it('minimize then focus restores the window', () => {
    let s = initialDesktop;
    s = open(s, { id: 'a' });
    s = desktopReducer(s, { type: 'minimize', id: 'a' });
    expect(s.windows[0].mode).toBe('minimized');
    s = desktopReducer(s, { type: 'focus', id: 'a' });
    expect(s.windows[0].mode).toBe('normal');
  });

  it('maximize saves restore geometry and toggling returns to it', () => {
    let s = initialDesktop;
    s = open(s, { id: 'a', geometry: { x: 10, y: 20, width: 300, height: 200 } });
    const before = s.windows[0].geometry;
    s = desktopReducer(s, {
      type: 'toggleMaximize',
      id: 'a',
      viewport: { width: 1920, height: 1080 },
    });
    expect(s.windows[0].mode).toBe('maximized');
    expect(s.windows[0].geometry).toEqual({ x: 0, y: 0, width: 1920, height: 1080 });
    s = desktopReducer(s, { type: 'toggleMaximize', id: 'a' });
    expect(s.windows[0].mode).toBe('normal');
    expect(s.windows[0].geometry).toEqual(before);
  });

  it("move and resize update only the target window's geometry", () => {
    let s = initialDesktop;
    s = open(s, { id: 'a' });
    s = open(s, { id: 'b' });
    s = desktopReducer(s, { type: 'move', id: 'a', x: 111, y: 222 });
    s = desktopReducer(s, {
      type: 'resize',
      id: 'b',
      geometry: { x: 1, y: 2, width: 333, height: 444 },
    });
    const a = s.windows.find((w) => w.id === 'a')!;
    const b = s.windows.find((w) => w.id === 'b')!;
    expect(a.geometry.x).toBe(111);
    expect(a.geometry.y).toBe(222);
    expect(b.geometry).toEqual({ x: 1, y: 2, width: 333, height: 444 });
  });

  it('close removes only the target window', () => {
    let s = initialDesktop;
    s = open(s, { id: 'a' });
    s = open(s, { id: 'b' });
    s = desktopReducer(s, { type: 'close', id: 'a' });
    expect(s.windows.map((w) => w.id)).toEqual(['b']);
  });

  it('dedupeKey collapses repeat opens onto the existing window', () => {
    let s = initialDesktop;
    s = desktopReducer(s, {
      type: 'open',
      spec: { id: 'settings', kind: 'settings', title: 'Settings', dedupeKey: 'settings' },
    });
    s = desktopReducer(s, { type: 'open', spec: { id: 'a', kind: 'placeholder', title: 'a' } });
    // Re-opening settings does not add a second window; it focuses the existing one.
    s = desktopReducer(s, {
      type: 'open',
      spec: { id: 'settings2', kind: 'settings', title: 'Settings', dedupeKey: 'settings' },
    });
    const settings = s.windows.filter((w) => w.kind === 'settings');
    expect(settings).toHaveLength(1);
    expect(settings[0].id).toBe('settings'); // the original, not the duplicate
    expect(settings[0].zIndex).toBe(s.topZ); // and it was focused
  });
});

describe('layout serialization', () => {
  it('round-trips through serialize → parse → hydrate preserving order and geometry', () => {
    let s = initialDesktop;
    s = desktopReducer(s, {
      type: 'open',
      spec: {
        id: 'term-1',
        kind: 'terminal',
        title: 'Terminal — alpha',
        projectId: '/workspace/alpha',
        session: 'win-1',
        cwd: '/workspace/alpha',
        geometry: { x: 50, y: 60, width: 700, height: 400 },
      },
    });
    s = desktopReducer(s, {
      type: 'open',
      spec: {
        id: 'ed-1',
        kind: 'editor',
        title: 'Editor — alpha',
        folder: '/workspace/alpha',
      },
    });

    const layout = serializeLayout(s);
    const json = JSON.stringify(layout);
    const parsed = parseLayout(json);
    expect(parsed).not.toBeNull();

    const hydrated = desktopReducer(initialDesktop, { type: 'hydrate', windows: parsed!.windows });
    expect(hydrated.windows.map((w) => w.id)).toEqual(['term-1', 'ed-1']);
    const term = hydrated.windows[0];
    expect(term.session).toBe('win-1');
    expect(term.cwd).toBe('/workspace/alpha');
    expect(term.geometry).toEqual({ x: 50, y: 60, width: 700, height: 400 });
    // Stacking re-derived by array order (last = top).
    expect(hydrated.windows[1].zIndex).toBeGreaterThan(hydrated.windows[0].zIndex);
  });

  it('excludes transient windows (projects launcher, placeholder) from the layout', () => {
    let s = initialDesktop;
    s = desktopReducer(s, {
      type: 'open',
      spec: { id: 'p', kind: 'projects', title: 'Projects' },
    });
    s = desktopReducer(s, {
      type: 'open',
      spec: { id: 'ph', kind: 'placeholder', title: 'x' },
    });
    s = desktopReducer(s, {
      type: 'open',
      spec: { id: 't', kind: 'terminal', title: 'Terminal', session: 's1' },
    });
    const layout = serializeLayout(s);
    expect(layout.windows.map((w) => w.id)).toEqual(['t']);
  });

  it('a maximized window persists at its restore geometry', () => {
    let s = initialDesktop;
    s = desktopReducer(s, {
      type: 'open',
      spec: {
        id: 't',
        kind: 'terminal',
        title: 'T',
        session: 's1',
        geometry: { x: 5, y: 6, width: 700, height: 400 },
      },
    });
    s = desktopReducer(s, {
      type: 'toggleMaximize',
      id: 't',
      viewport: { width: 1920, height: 1080 },
    });
    const layout = serializeLayout(s);
    expect(layout.windows[0].geometry).toEqual({ x: 5, y: 6, width: 700, height: 400 });
    expect(layout.windows[0].mode).toBe('normal');
  });

  it('hydrate keeps the existing projects launcher beneath restored windows', () => {
    // The launcher is opened on mount; an async layout hydrate must not wipe it.
    let s = desktopReducer(initialDesktop, {
      type: 'open',
      spec: { id: 'projects', kind: 'projects', title: 'Projects', dedupeKey: 'projects' },
    });
    s = desktopReducer(s, {
      type: 'hydrate',
      windows: [
        {
          id: 'term-1',
          kind: 'terminal',
          title: 'Terminal',
          session: 's1',
          geometry: { x: 0, y: 0, width: 700, height: 400 },
        },
      ],
    });
    expect(s.windows.map((w) => w.id)).toEqual(['projects', 'term-1']);
    // The restored terminal stacks above the kept launcher.
    expect(s.windows[1].zIndex).toBeGreaterThan(s.windows[0].zIndex);
  });

  it('scopes dedupe per machine (projects launcher is one per machine)', () => {
    let s = initialDesktop;
    s = open(s, { id: 'projects-m1', kind: 'projects', machineId: 'm1', dedupeKey: 'projects|m1' });
    s = open(s, { id: 'projects-m2', kind: 'projects', machineId: 'm2', dedupeKey: 'projects|m2' });
    // Different machines ⇒ two launchers coexist.
    expect(s.windows.map((w) => w.id)).toEqual(['projects-m1', 'projects-m2']);
    // Re-opening m1's launcher collapses onto the existing one (no third window).
    s = open(s, {
      id: 'projects-m1-again',
      kind: 'projects',
      machineId: 'm1',
      dedupeKey: 'projects|m1',
    });
    expect(s.windows.map((w) => w.id)).toEqual(['projects-m1', 'projects-m2']);
  });

  it('scopes preview dedupe per (machine, port) so switching ports leaves prior previews open', () => {
    let s = initialDesktop;
    s = open(s, {
      id: 'preview-m1-3000',
      kind: 'preview',
      machineId: 'm1',
      port: 3000,
      dedupeKey: 'm1|3000',
    });
    s = open(s, {
      id: 'preview-m1-8080',
      kind: 'preview',
      machineId: 'm1',
      port: 8080,
      dedupeKey: 'm1|8080',
    });
    // A different port on the same machine opens its own window (prior preview unaffected).
    expect(s.windows.map((w) => w.id)).toEqual(['preview-m1-3000', 'preview-m1-8080']);
    // Re-opening port 3000 collapses onto the existing window (no third).
    s = open(s, {
      id: 'preview-m1-3000-again',
      kind: 'preview',
      machineId: 'm1',
      port: 3000,
      dedupeKey: 'm1|3000',
    });
    expect(s.windows.map((w) => w.id)).toEqual(['preview-m1-3000', 'preview-m1-8080']);
  });

  it('scopes session-detail dedupe per session id, and excludes it from the persisted layout', () => {
    let s = initialDesktop;
    s = open(s, {
      id: 'session-detail-s1',
      kind: 'session-detail',
      sessionId: 's1',
      dedupeKey: 'session-detail|s1',
    });
    s = open(s, {
      id: 'session-detail-s2',
      kind: 'session-detail',
      sessionId: 's2',
      dedupeKey: 'session-detail|s2',
    });
    // A different session opens its own window (prior detail window unaffected).
    expect(s.windows.map((w) => w.id)).toEqual(['session-detail-s1', 'session-detail-s2']);
    // Re-opening the same session collapses onto the existing window (no third).
    s = open(s, {
      id: 'session-detail-s1-again',
      kind: 'session-detail',
      sessionId: 's1',
      dedupeKey: 'session-detail|s1',
    });
    expect(s.windows.map((w) => w.id)).toEqual(['session-detail-s1', 'session-detail-s2']);
    // Ephemeral like Tasks/Changes: never persisted, so a reload does not restore it.
    expect(serializeLayout(s).windows.map((w) => w.kind)).not.toContain('session-detail');
  });

  it('round-trips a preview window port through serialize → parse → hydrate', () => {
    let s = initialDesktop;
    s = open(s, {
      id: 'preview-m1-3000',
      kind: 'preview',
      title: 'App — port 3000',
      machineId: 'm1',
      port: 3000,
    });
    const parsed = parseLayout(JSON.stringify(serializeLayout(s)));
    const hydrated = desktopReducer(initialDesktop, { type: 'hydrate', windows: parsed!.windows });
    const win = hydrated.windows.find((w) => w.id === 'preview-m1-3000')!;
    expect(win.kind).toBe('preview');
    expect(win.port).toBe(3000);
  });

  it('hydrateMachine restores one machine without disturbing another', () => {
    let s = initialDesktop;
    // A live terminal on m2 that must survive m1 being (re)hydrated.
    s = open(s, { id: 't-m2', kind: 'terminal', machineId: 'm2', session: 's2' });
    s = desktopReducer(s, {
      type: 'hydrateMachine',
      machineId: 'm1',
      windows: [
        {
          id: 't-m1',
          kind: 'terminal',
          title: 'T1',
          machineId: 'm1',
          session: 's1',
          geometry: { x: 10, y: 10, width: 400, height: 300 },
        },
      ],
    });
    const ids = s.windows.map((w) => w.id).sort();
    expect(ids).toEqual(['t-m1', 't-m2']);
    // m2's window kept its machineId/session (untouched).
    const m2 = s.windows.find((w) => w.id === 't-m2')!;
    expect(m2.machineId).toBe('m2');
    expect(m2.session).toBe('s2');
  });

  it('serializeLayout can scope to one machine', () => {
    let s = initialDesktop;
    s = open(s, { id: 't-m1', kind: 'terminal', machineId: 'm1', session: 's1' });
    s = open(s, { id: 't-m2', kind: 'terminal', machineId: 'm2', session: 's2' });
    const onlyM1 = serializeLayout(s, 'm1');
    expect(onlyM1.windows.map((w) => w.id)).toEqual(['t-m1']);
    expect(onlyM1.windows[0].machineId).toBe('m1');
  });

  it('parseLayout rejects malformed input', () => {
    expect(parseLayout(null)).toBeNull();
    expect(parseLayout('not json')).toBeNull();
    expect(parseLayout(JSON.stringify({ version: 2, windows: [] }))).toBeNull();
    expect(parseLayout(JSON.stringify({ version: 1 }))).toBeNull();
    // Valid envelope with a junk window entry filters the bad entry out.
    const ok = parseLayout(
      JSON.stringify({
        version: 1,
        windows: [
          { id: 'x' },
          { id: 'y', kind: 'terminal', title: 'Y', geometry: { x: 0, y: 0, width: 1, height: 1 } },
        ],
      }),
    );
    expect(ok?.windows.map((w) => w.id)).toEqual(['y']);
  });
});
