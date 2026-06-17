// @vitest-environment jsdom
import { useEffect } from 'react';
import { act } from 'react';
import { createRoot } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { WindowManagerProvider, useWindowManager } from './WindowManager';

// React needs this flag to recognize our act(...) wrapping under vitest.
(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

// This test pins the mount-once invariant (decision #2): focusing or moving a
// window must NOT remount its content subtree. It asserts at the manager level —
// windows are keyed by a stable id and the array is never reordered, so a probe
// mounted inside one window's content mounts exactly once across focus/move/min/
// max of itself or a sibling. (react-rnd's pointer math is exercised in the live
// app; here we isolate the React identity guarantee the reducer provides.)

let mountCount = 0;

function MountProbe() {
  useEffect(() => {
    mountCount += 1;
  }, []);
  return <span data-probe />;
}

let actions: ReturnType<typeof useWindowManager> | null = null;

function Harness() {
  const wm = useWindowManager();
  actions = wm;
  return (
    <div>
      {wm.windows.map((w) => (
        <div key={w.id} style={{ zIndex: w.zIndex }}>
          {w.id === 'a' ? <MountProbe /> : <span>{w.id}</span>}
        </div>
      ))}
    </div>
  );
}

let container: HTMLDivElement;
let root: ReturnType<typeof createRoot>;

beforeEach(() => {
  mountCount = 0;
  actions = null;
  container = document.createElement('div');
  document.body.appendChild(container);
  root = createRoot(container);
  act(() => {
    root.render(
      <WindowManagerProvider>
        <Harness />
      </WindowManagerProvider>,
    );
  });
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
});

describe('mount-once invariant', () => {
  it('does not remount window content on open of a sibling, focus, move, or maximize', () => {
    act(() => actions!.open({ id: 'a', kind: 'placeholder', title: 'a' }));
    expect(mountCount).toBe(1);

    // Opening a sibling must not remount window a.
    act(() => actions!.open({ id: 'b', kind: 'placeholder', title: 'b' }));
    expect(mountCount).toBe(1);

    // Focus a (raises z-order) — content stays mounted.
    act(() => actions!.focus('a'));
    expect(mountCount).toBe(1);

    // Focus the sibling (changes topZ but not a's subtree).
    act(() => actions!.focus('b'));
    expect(mountCount).toBe(1);

    // Move and maximize a — geometry/mode changes, never a remount.
    act(() => actions!.move('a', 50, 60));
    act(() => actions!.toggleMaximize('a', { width: 800, height: 600 }));
    act(() => actions!.minimize('a'));
    act(() => actions!.focus('a'));
    expect(mountCount).toBe(1);
  });
});
