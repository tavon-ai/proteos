import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from 'react';
import type { MachineSummary } from '../api/client';

// SelectedMachine is the desktop's "active machine" — multi-machine UX shows one
// machine's desktop at a time (decision: active machine + switcher). The active
// id is persisted to localStorage so a reload keeps the same machine selected.

const STORAGE_KEY = 'proteos.selectedMachine';

interface SelectedMachine {
  machines: MachineSummary[];
  selected: MachineSummary | null;
  selectedId: string | null;
  setSelectedId: (id: string) => void;
}

const Ctx = createContext<SelectedMachine | null>(null);

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

function readPersisted(): string | null {
  try {
    return localStorage.getItem(STORAGE_KEY);
  } catch {
    return null;
  }
}

export function SelectedMachineProvider({
  machines,
  children,
}: {
  machines: MachineSummary[];
  children: ReactNode;
}) {
  const [selectedId, setSelectedIdState] = useState<string | null>(() =>
    chooseDefaultMachine(machines, readPersisted()),
  );

  // Keep the selection valid as the list changes (the selected machine was
  // destroyed, or the user's first machine just arrived over SSE).
  useEffect(() => {
    setSelectedIdState((cur) => (cur && machines.some((m) => m.id === cur) ? cur : chooseDefaultMachine(machines, cur)));
  }, [machines]);

  const setSelectedId = useCallback((id: string) => {
    setSelectedIdState(id);
    try {
      localStorage.setItem(STORAGE_KEY, id);
    } catch {
      /* ignore: selection just won't persist across reloads */
    }
  }, []);

  const value = useMemo<SelectedMachine>(() => {
    const selected = machines.find((m) => m.id === selectedId) ?? null;
    return { machines, selected, selectedId: selected?.id ?? null, setSelectedId };
  }, [machines, selectedId, setSelectedId]);

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useSelectedMachine(): SelectedMachine {
  const ctx = useContext(Ctx);
  if (!ctx) throw new Error('useSelectedMachine must be used within a SelectedMachineProvider');
  return ctx;
}
