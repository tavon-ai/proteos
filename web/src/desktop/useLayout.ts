import { useCallback, useEffect, useRef } from 'react';
import { api } from '../api/client';
import { serializeLayout, type DesktopState } from './windowState';
import type { WindowManagerContext } from './windowManagerContext';
import { parseLayout } from './windowState';

const SAVE_DEBOUNCE_MS = 1000;

// useLayoutSaver returns a debounced save callback wired to the window manager's
// onChange. It serializes only the ACTIVE machine's windows and PUTs them to that
// machine's SQLite (decision #6) — each machine persists its own layout. Saves
// are skipped while the active machine is not running/known (the endpoint 409s);
// a diskless stack accepts the PUT as a no-op. Failures are swallowed — a dropped
// layout save must never disrupt the UI.
export function useLayoutSaver(
  machineId: string | null,
  running: boolean,
): (state: DesktopState) => void {
  const timer = useRef<ReturnType<typeof setTimeout>>();
  useEffect(() => () => clearTimeout(timer.current), []);
  return useCallback(
    (state: DesktopState) => {
      if (!running || !machineId) return;
      clearTimeout(timer.current);
      timer.current = setTimeout(() => {
        api.putDesktop(machineId, serializeLayout(state, machineId)).catch(() => {
          /* layout persistence is best-effort */
        });
      }, SAVE_DEBOUNCE_MS);
    },
    [machineId, running],
  );
}

// useLayoutLoader restores a machine's saved layout the first time it becomes the
// active running machine, via hydrateMachine — which only touches that machine's
// windows, leaving every other machine's live windows (and their terminals)
// untouched. Each machine is loaded at most once per page load (tracked in a
// ref), so switching back and forth never wipes in-session state. A corrupt or
// absent layout leaves that machine's desktop clean (the launcher still opens).
export function useLayoutLoader(
  wm: WindowManagerContext,
  machineId: string | null,
  running: boolean,
): void {
  const loaded = useRef<Set<string>>(new Set());
  useEffect(() => {
    if (!running || !machineId || loaded.current.has(machineId)) return;
    loaded.current.add(machineId);
    let cancelled = false;
    api
      .getDesktop(machineId)
      .then((r) => {
        if (cancelled) return;
        const layout = parseLayout(r.layout);
        if (layout && layout.windows.length > 0) wm.hydrateMachine(machineId, layout.windows);
      })
      .catch(() => {
        // No saved layout / guest unreachable — allow a later retry for this machine.
        loaded.current.delete(machineId);
      });
    return () => {
      cancelled = true;
    };
    // wm is stable (provider ref); keying on machineId+running drives loading.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [machineId, running]);
}
