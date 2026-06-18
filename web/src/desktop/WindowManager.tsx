import { useCallback, useMemo, useReducer, useRef, type ReactNode } from 'react';
import { desktopReducer, initialDesktop, type DesktopState } from './windowState';
import { WindowManagerCtx, type WindowManagerContext } from './windowManagerContext';

// WindowManager is the React shell over the pure desktopReducer. It owns the
// window registry and exposes imperative actions to the shell/windows; the heavy
// lifting (z-order, cascade, min/max, layout (de)serialization) lives in
// windowState so it stays testable. An `onChange` callback fires after every
// structural mutation so the desktop can debounce-save the layout (Phase 9 #6).
// The context object and useWindowManager hook live in windowManagerContext so
// this file exports only the provider component (required for Fast Refresh).

export function WindowManagerProvider({
  children,
  onChange,
}: {
  children: ReactNode;
  // Called after every structural change (open/close/move/resize/min/max), with
  // the new state. The desktop debounces this into a layout save.
  onChange?: (state: DesktopState) => void;
}) {
  const [state, dispatch] = useReducer(desktopReducer, initialDesktop);

  // Keep the latest onChange without re-creating the action callbacks.
  const onChangeRef = useRef(onChange);
  onChangeRef.current = onChange;

  // After each commit, notify the saver. We read state through a ref captured at
  // dispatch time via a microtask so onChange sees the post-reducer value.
  const stateRef = useRef(state);
  stateRef.current = state;

  const notify = useCallback(() => {
    // Defer so the reducer has committed and stateRef points at the new state.
    queueMicrotask(() => onChangeRef.current?.(stateRef.current));
  }, []);

  const ctx = useMemo<WindowManagerContext>(
    () => ({
      windows: state.windows,
      topZ: state.topZ,
      state,
      open: (spec) => {
        dispatch({ type: 'open', spec });
        notify();
      },
      close: (id) => {
        dispatch({ type: 'close', id });
        notify();
      },
      focus: (id) => dispatch({ type: 'focus', id }),
      move: (id, x, y) => {
        dispatch({ type: 'move', id, x, y });
        notify();
      },
      resize: (id, geometry) => {
        dispatch({ type: 'resize', id, geometry });
        notify();
      },
      minimize: (id) => {
        dispatch({ type: 'minimize', id });
        notify();
      },
      toggleMaximize: (id, viewport) => {
        dispatch({ type: 'toggleMaximize', id, viewport });
        notify();
      },
      restore: (id) => {
        dispatch({ type: 'restore', id });
        notify();
      },
      hydrate: (windows) => dispatch({ type: 'hydrate', windows }),
      hydrateMachine: (machineId, windows) => dispatch({ type: 'hydrateMachine', machineId, windows }),
    }),
    [state, notify],
  );

  return <WindowManagerCtx.Provider value={ctx}>{children}</WindowManagerCtx.Provider>;
}
