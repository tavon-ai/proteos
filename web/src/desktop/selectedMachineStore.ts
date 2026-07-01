import { createContext, useContext } from 'react';
import type { MachineSummary } from '../api/client';

// SelectedMachine is the desktop's "active machine" — multi-machine UX shows one
// machine's desktop at a time (decision: active machine + switcher). The active
// id is persisted to localStorage so a reload keeps the same machine selected.

export const STORAGE_KEY = 'proteos.selectedMachine';

export interface SelectedMachine {
  machines: MachineSummary[];
  selected: MachineSummary | null;
  selectedId: string | null;
  setSelectedId: (id: string) => void;
}

export const SelectedMachineContext = createContext<SelectedMachine | null>(null);

// chooseDefaultMachine picks the active machine when none is validly selected: a
// persisted id if still present, else the first running machine, else the first
// machine, else none. Exported for unit testing.
export function chooseDefaultMachine(
  machines: MachineSummary[],
  persisted: string | null,
): string | null {
  if (persisted && machines.some((m) => m.id === persisted)) return persisted;
  const running = machines.find((m) => m.state === 'running');
  if (running) return running.id;
  return machines[0]?.id ?? null;
}

export function readPersisted(): string | null {
  try {
    return localStorage.getItem(STORAGE_KEY);
  } catch {
    return null;
  }
}

export function useSelectedMachine(): SelectedMachine {
  const ctx = useContext(SelectedMachineContext);
  if (!ctx) throw new Error('useSelectedMachine must be used within a SelectedMachineProvider');
  return ctx;
}
