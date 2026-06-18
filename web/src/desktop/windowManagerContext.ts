import { createContext, useContext } from 'react';
import type { DesktopState, Geometry, OpenSpec, PersistedWindow, WindowState } from './windowState';

// The WindowManager context surface and its consumer hook live here, apart from
// the WindowManagerProvider component. Keeping the non-component exports (the
// context object and the useWindowManager hook) out of WindowManager.tsx lets
// that file export only components, which is what React Fast Refresh requires.

export interface WindowManagerContext {
  windows: WindowState[];
  topZ: number;
  open: (spec: OpenSpec) => void;
  close: (id: string) => void;
  focus: (id: string) => void;
  move: (id: string, x: number, y: number) => void;
  resize: (id: string, geometry: Geometry) => void;
  minimize: (id: string) => void;
  toggleMaximize: (id: string, viewport?: { width: number; height: number }) => void;
  restore: (id: string) => void;
  hydrate: (windows: PersistedWindow[]) => void;
  /** Restore one machine's windows without disturbing other machines'. */
  hydrateMachine: (machineId: string, windows: PersistedWindow[]) => void;
  /** Current full state — used by the layout saver to serialize. */
  state: DesktopState;
}

export const WindowManagerCtx = createContext<WindowManagerContext | null>(null);

export function useWindowManager(): WindowManagerContext {
  const ctx = useContext(WindowManagerCtx);
  if (!ctx) throw new Error('useWindowManager must be used within a WindowManagerProvider');
  return ctx;
}
