import { useCallback, useEffect, useRef } from "react";
import { api } from "../api/client";
import { serializeLayout, type DesktopState } from "./windowState";
import type { WindowManagerContext } from "./WindowManager";
import { parseLayout } from "./windowState";

const SAVE_DEBOUNCE_MS = 1000;

// useLayoutSaver returns a debounced save callback wired to the window manager's
// onChange. It serializes the live desktop and PUTs it to machine SQLite
// (decision #6). Saves are skipped while the machine is not running (the endpoint
// 409s and there is nothing to persist to); a diskless stack accepts the PUT as a
// no-op. Failures are swallowed — a dropped layout save must never disrupt the UI.
export function useLayoutSaver(running: boolean): (state: DesktopState) => void {
  const timer = useRef<ReturnType<typeof setTimeout>>();
  useEffect(() => () => clearTimeout(timer.current), []);
  return useCallback(
    (state: DesktopState) => {
      if (!running) return;
      clearTimeout(timer.current);
      timer.current = setTimeout(() => {
        api.putDesktop(serializeLayout(state)).catch(() => {
          /* layout persistence is best-effort */
        });
      }, SAVE_DEBOUNCE_MS);
    },
    [running],
  );
}

// useLayoutLoader hydrates the desktop from machine SQLite once per running
// transition. Restored terminal/agent windows reconnect to their live PTYs by
// the opaque session id stored in the layout (Phase 3 reconnect). A corrupt or
// absent layout leaves the desktop empty (the launcher still opens).
export function useLayoutLoader(wm: WindowManagerContext, running: boolean): void {
  const loadedRef = useRef(false);
  useEffect(() => {
    if (!running) {
      loadedRef.current = false; // re-hydrate after a stop/start
      return;
    }
    if (loadedRef.current) return;
    loadedRef.current = true;
    let cancelled = false;
    api
      .getDesktop()
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
  }, [running, wm]);
}
