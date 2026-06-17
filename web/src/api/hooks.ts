import { useEffect, useRef, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  api,
  ApiError,
  machineEventsUrl,
  SessionExpiredError,
  type MachineEvent,
  type MachineEventData,
  type MachineSummary,
  type ProjectsResponse,
  type ReposResponse,
  type SnapshotData,
} from './client';
import { logger } from '../lib/logger';

const log = logger.child({ component: 'machine-events' });

// useMe loads the current user. A 401 (SessionExpiredError) is NOT retried —
// it is the normal "not logged in" signal, consumed by the route guard.
export function useMe() {
  return useQuery({
    queryKey: ['me'],
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
const machineKey = ['machine'] as const;

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
const providersKey = ['providers'] as const;

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
    mutationFn: ({ key, fields }: { key: string; fields: Record<string, string> }) =>
      api.setProviderKey(key, fields),
    onSuccess: invalidate,
  });
  const deleteKey = useMutation({
    mutationFn: (key: string) => api.deleteProviderKey(key),
    onSuccess: invalidate,
  });
  return { setKey, deleteKey };
}

// reposKey is the query cache key for the user's accessible repos.
const reposKey = ['repos'] as const;

// useRepos loads the repos the user has granted the GitHub App access to (Phase
// 7). A 409 reconnect_github (ApiError) is surfaced — not retried — so the UI can
// show the Reconnect banner; a 401 still routes to /login. The data is not
// refetched aggressively (repo lists change slowly); the panel offers a manual
// refresh and React Query refetches on window focus.
export function useRepos() {
  return useQuery<ReposResponse>({
    queryKey: reposKey,
    queryFn: api.listRepos,
    retry: (failureCount, error) => {
      if (error instanceof SessionExpiredError) return false;
      if (error instanceof ApiError) return false; // 409 reconnect / 5xx: don't hammer
      return failureCount < 2;
    },
  });
}

// projectsKey is the query cache key for the machine's cloned projects.
const projectsKey = ['projects'] as const;

// useProjects loads the machine's cloned projects (Phase 9). Disabled until the
// machine is running (the endpoint 409s otherwise); refetched imperatively on the
// git.clone SSE event via invalidateProjects below.
export function useProjects(enabled: boolean) {
  return useQuery<ProjectsResponse>({
    queryKey: projectsKey,
    queryFn: api.listProjects,
    enabled,
    retry: (failureCount, error) => {
      if (error instanceof SessionExpiredError) return false;
      if (error instanceof ApiError) return false; // 409 not-running: don't hammer
      return failureCount < 2;
    },
  });
}

// useInvalidateProjects returns a function that forces a projects refetch — wired
// to the git.clone machine event so a finished clone surfaces its new tile.
export function useInvalidateProjects() {
  const qc = useQueryClient();
  return () => qc.invalidateQueries({ queryKey: projectsKey });
}

// useCloneRepo dispatches a clone. It returns the op_id immediately (202);
// completion arrives as a git.clone machine event. On a stale grant the mutation
// rejects with ApiError 409 reconnect_github, which the panel surfaces.
export function useCloneRepo() {
  return useMutation({
    mutationFn: (fullName: string) => api.cloneRepo(fullName),
  });
}

// reconnectRequired reports whether an error is the GitHub "reconnect" signal.
export function reconnectRequired(error: unknown): boolean {
  return error instanceof ApiError && error.status === 409 && error.code === 'reconnect_github';
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

    es.addEventListener('open', () => log.info('stream open'));

    es.addEventListener('snapshot', (e) => {
      const data = JSON.parse((e as MessageEvent).data) as SnapshotData;
      qc.setQueryData(machineKey, data.machine);
      // Snapshot events arrive oldest-first; show newest-first.
      for (const ev of data.events) pushEvent(ev);
      log.debug('snapshot', { state: data.machine?.state ?? null, events: data.events.length });
    });

    es.addEventListener('machine', (e) => {
      const data = JSON.parse((e as MessageEvent).data) as MachineEventData;
      qc.setQueryData(machineKey, data.machine);
      pushEvent(data.event);
      log.debug('event', { type: data.event.type, to: data.event.to_state });
    });

    // EventSource auto-reconnects on error; surface it so a flapping stream is
    // visible (readyState 2 = closed, e.g. auth lost).
    es.addEventListener('error', () => {
      log.warn('stream error', { readyState: es.readyState });
    });

    return () => es.close();
  }, [qc]);

  return events;
}
