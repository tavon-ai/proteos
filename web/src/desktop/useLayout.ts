import { useCallback, useEffect, useRef } from 'react';
import { api } from '../api/client';
import { serializeLayout, type DesktopState } from './windowState';
import type { WindowManagerContext } from './windowManagerContext';
import { parseLayout } from './windowState';

const SAVE_DEBOUNCE_MS = 1000;

// useLayoutSaver returns a debounced save callback wired to the window manager's
// onChange. It serializes the live desktop and PUTs it to the active machine's
// SQLite (decision #6). Saves are skipped while the machine is not running/known
// (the endpoint 409s and there is nothing to persist to); a diskless stack
// accepts the PUT as a no-op. Failures are swallowed — a dropped layout save must
// never disrupt the UI.
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
        api.putDesktop(machineId, serializeLayout(state)).catch(() => {
          /* layout persistence is best-effort */
        });
      }, SAVE_DEBOUNCE_MS);
    },
    [machineId, running],
  );
}

// useLayoutLoader scopes the desktop to the active machine. On any machine switch
// it resets to a clean desktop (hydrate([]) keeps the Projects launcher but
// clears the previous machine's terminals/editors), then — once that machine is
// running — hydrates its saved layout, reconnecting live PTYs by their opaque
// session ids (Phase 3 reconnect). A corrupt or absent layout leaves the desktop
// clean (the launcher still opens).
export function useLayoutLoader(
  wm: WindowManagerContext,
  machineId: string | null,
  running: boolean,
): void {
  useEffect(() => {
    // Reset to a clean desktop for the (possibly new) active machine.
    wm.hydrate([]);
    if (!running || !machineId) return;
    let cancelled = false;
    api
      .getDesktop(machineId)
      .then((r) => {
        if (cancelled) return;
        const layout = parseLayout(r.layout);
        if (layout && layout.windows.length > 0) wm.hydrate(layout.windows);
      })
      .catch(() => {
        /* no saved layout / guest unreachable — start clean */
      });
    return () => {
      cancelled = true;
    };
    // wm is stable (provider ref); keying on machineId+running drives re-scoping.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [machineId, running]);
}
