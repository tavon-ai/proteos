import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  api,
  machineEventsUrl,
  SessionExpiredError,
  type MachineEvent,
  type MachineEventData,
  type MachineSummary,
  type SnapshotData,
} from "./client";

// useMe loads the current user. A 401 (SessionExpiredError) is NOT retried —
// it is the normal "not logged in" signal, consumed by the route guard.
export function useMe() {
  return useQuery({
    queryKey: ["me"],
    queryFn: api.me,
    retry: (failureCount, error) => {
      if (error instanceof SessionExpiredError) return false;
      return failureCount < 2;
    },
  });
}

export function useLogout() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: api.logout,
    onSuccess: () => {
      qc.clear();
    },
  });
}

// machineKey is the query cache key for the user's machine.
const machineKey = ["machine"] as const;

// useMachine loads the user's machine (null if none). Seeded from /api/me on
// first paint so the dashboard renders without a second round-trip; the SSE
// stream then keeps it live.
export function useMachine(initial: MachineSummary | null) {
  return useQuery({
    queryKey: machineKey,
    queryFn: api.getMachine,
    initialData: initial,
    retry: (failureCount, error) => {
      if (error instanceof SessionExpiredError) return false;
      return failureCount < 2;
    },
  });
}

// useMachineMutations exposes create/start/stop. Each writes the returned
// summary straight into the machine query cache so the UI reflects the new
// (transitional) state immediately, before the first SSE event arrives.
export function useMachineMutations() {
  const qc = useQueryClient();
  const onSuccess = (m: MachineSummary) => qc.setQueryData(machineKey, m);

  const create = useMutation({ mutationFn: api.createMachine, onSuccess });
  const start = useMutation({ mutationFn: api.startMachine, onSuccess });
  const stop = useMutation({ mutationFn: api.stopMachine, onSuccess });
  return { create, start, stop };
}

// providersKey is the query cache key for the provider registry + key_set view.
const providersKey = ["providers"] as const;

// useProviders loads the provider registry with the caller's key_set status.
export function useProviders() {
  return useQuery({
    queryKey: providersKey,
    queryFn: api.listProviders,
    retry: (failureCount, error) => {
      if (error instanceof SessionExpiredError) return false;
      return failureCount < 2;
    },
  });
}

// useProviderMutations exposes set/delete of a provider's write-only key. Both
// invalidate the providers query so key_set re-renders from the server (the key
// itself is never held client-side).
export function useProviderMutations() {
  const qc = useQueryClient();
  const invalidate = () => qc.invalidateQueries({ queryKey: providersKey });

  const setKey = useMutation({
    mutationFn: ({ key, apiKey }: { key: string; apiKey: string }) =>
      api.setProviderKey(key, apiKey),
    onSuccess: invalidate,
  });
  const deleteKey = useMutation({
    mutationFn: (key: string) => api.deleteProviderKey(key),
    onSuccess: invalidate,
  });
  return { setKey, deleteKey };
}

// useMachineEvents subscribes to the SSE stream. It writes live machine state
// into the query cache (so useMachine stays current without polling) and keeps
// a rolling event log for display. The browser EventSource reconnects on its
// own; it replays Last-Event-ID automatically, and our server backfills the
// missed rows.
export function useMachineEvents(): MachineEvent[] {
  const qc = useQueryClient();
  const [events, setEvents] = useState<MachineEvent[]>([]);
  // Guard against duplicate ids across reconnect replays.
  const seen = useRef<Set<number>>(new Set());

  useEffect(() => {
    const es = new EventSource(machineEventsUrl, { withCredentials: true });

    const pushEvent = (ev: MachineEvent) => {
      if (seen.current.has(ev.id)) return;
      seen.current.add(ev.id);
      setEvents((prev) => [ev, ...prev].slice(0, 100));
    };

    es.addEventListener("snapshot", (e) => {
      const data = JSON.parse((e as MessageEvent).data) as SnapshotData;
      qc.setQueryData(machineKey, data.machine);
      // Snapshot events arrive oldest-first; show newest-first.
      for (const ev of data.events) pushEvent(ev);
    });

    es.addEventListener("machine", (e) => {
      const data = JSON.parse((e as MessageEvent).data) as MachineEventData;
      qc.setQueryData(machineKey, data.machine);
      pushEvent(data.event);
    });

    // On error EventSource auto-reconnects; nothing to do but let it.
    return () => es.close();
  }, [qc]);

  return events;
}
