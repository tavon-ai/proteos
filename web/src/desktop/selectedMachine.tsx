import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';
import {
  SelectedMachineContext,
  STORAGE_KEY,
  chooseDefaultMachine,
  readPersisted,
  type SelectedMachine,
} from './selectedMachineStore';
import type { MachineSummary } from '../api/client';

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
    setSelectedIdState((cur) =>
      cur && machines.some((m) => m.id === cur) ? cur : chooseDefaultMachine(machines, cur),
    );
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

  return (
    <SelectedMachineContext.Provider value={value}>{children}</SelectedMachineContext.Provider>
  );
}
