import { useEffect, useRef, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  api,
  ApiError,
  machineEventsUrl,
  SessionExpiredError,
  type CreateMachineInput,
  type MachineDestroyedData,
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

// machinesKey is the query cache key for the user's list of machines.
const machinesKey = ['machines'] as const;

// useMachines loads all of the user's machines. Seeded from /api/me on first
// paint so the desktop renders without a second round-trip; the SSE stream then
// keeps the list live (upsert/remove by id).
export function useMachines(initial: MachineSummary[]) {
  return useQuery({
    queryKey: machinesKey,
    queryFn: api.listMachines,
    initialData: initial,
    retry: (failureCount, error) => {
      if (error instanceof SessionExpiredError) return false;
      return failureCount < 2;
    },
  });
}

// upsertMachine merges a single machine summary into the cached list by id.
function upsertMachine(qc: ReturnType<typeof useQueryClient>, m: MachineSummary) {
  qc.setQueryData<MachineSummary[]>(machinesKey, (prev = []) => {
    const i = prev.findIndex((x) => x.id === m.id);
    if (i === -1) return [...prev, m];
    const next = prev.slice();
    next[i] = m;
    return next;
  });
}

// removeMachine drops a machine from the cached list by id.
function removeMachine(qc: ReturnType<typeof useQueryClient>, id: string) {
  qc.setQueryData<MachineSummary[]>(machinesKey, (prev = []) => prev.filter((x) => x.id !== id));
}

// templatesKey is the query cache key for the machine-template catalog.
const templatesKey = ['templates'] as const;

// useTemplates loads the machine-template catalog. It is static within a deploy,
// so it never goes stale on its own (the create dialog reads it on demand).
export function useTemplates() {
  return useQuery({
    queryKey: templatesKey,
    queryFn: api.getTemplates,
    staleTime: Infinity,
    retry: (failureCount, error) => {
      if (error instanceof SessionExpiredError) return false;
      return failureCount < 2;
    },
  });
}

// useMachineMutations exposes create/start/stop/destroy/rename. create takes an
// optional CreateMachineInput (name + template + resource overrides); the rest
// are keyed by machine id. Successful results are merged into the machines list
// cache immediately, before the first SSE event arrives.
export function useMachineMutations() {
  const qc = useQueryClient();
  const onSuccess = (m: MachineSummary) => upsertMachine(qc, m);

  const create = useMutation({
    mutationFn: (input?: CreateMachineInput) => api.createMachine(input),
    onSuccess,
  });
  const start = useMutation({ mutationFn: (id: string) => api.startMachine(id), onSuccess });
  const stop = useMutation({ mutationFn: (id: string) => api.stopMachine(id), onSuccess });
  const rename = useMutation({
    mutationFn: ({ id, name }: { id: string; name: string }) => api.renameMachine(id, name),
    onSuccess,
  });
  // Destroy returns 204 (no body); drop it from the cache immediately, before the
  // SSE `destroyed` event lands.
  const destroy = useMutation({
    mutationFn: (id: string) => api.destroyMachine(id),
    onSuccess: (_void, id) => removeMachine(qc, id),
  });
  return { create, start, stop, rename, destroy };
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

// projectsKey is the query cache key for a machine's cloned projects (keyed by
// machine id so switching machines refetches the right set).
const projectsKey = (machineId: string | null) => ['projects', machineId] as const;

// useProjects loads a machine's cloned projects (Phase 9). Disabled until the
// machine is running and known (the endpoint 409s otherwise); refetched
// imperatively on the git.clone SSE event via useInvalidateProjects below.
export function useProjects(machineId: string | null, enabled: boolean) {
  return useQuery<ProjectsResponse>({
    queryKey: projectsKey(machineId),
    queryFn: () => api.listProjects(machineId as string),
    enabled: enabled && !!machineId,
    retry: (failureCount, error) => {
      if (error instanceof SessionExpiredError) return false;
      if (error instanceof ApiError) return false; // 409 not-running: don't hammer
      return failureCount < 2;
    },
  });
}

// useInvalidateProjects returns a function that forces a projects refetch — wired
// to the git.clone machine event so a finished clone surfaces its new tile. It
// invalidates every machine's projects query (the event does not name which).
export function useInvalidateProjects() {
  const qc = useQueryClient();
  return () => qc.invalidateQueries({ queryKey: ['projects'] });
}

// useCloneRepo dispatches a clone into the given machine. It returns the op_id
// immediately (202); completion arrives as a git.clone machine event. On a stale
// grant the mutation rejects with ApiError 409 reconnect_github.
export function useCloneRepo(machineId: string | null) {
  return useMutation({
    mutationFn: (fullName: string) => api.cloneRepo(machineId as string, fullName),
  });
}

// Worktree review (GR1): a project's git status / unified diff, keyed by
// (machine, project) so switching either refetches. Both 409 when the machine is
// not running and 400 on a bad project — ApiError, not retried. The Changes
// window refetches on demand (a Refresh button) and on the git.clone SSE event.
const gitStatusKey = (machineId: string | null, project: string) =>
  ['git-status', machineId, project] as const;
const gitDiffKey = (machineId: string | null, project: string, staged: boolean) =>
  ['git-diff', machineId, project, staged] as const;

export function useGitStatus(machineId: string | null, project: string, enabled: boolean) {
  return useQuery({
    queryKey: gitStatusKey(machineId, project),
    queryFn: () => api.gitStatus(machineId as string, project),
    enabled: enabled && !!machineId && !!project,
    retry: (failureCount, error) => {
      if (error instanceof SessionExpiredError) return false;
      if (error instanceof ApiError) return false;
      return failureCount < 2;
    },
  });
}

export function useGitDiff(
  machineId: string | null,
  project: string,
  staged: boolean,
  enabled: boolean,
) {
  return useQuery({
    queryKey: gitDiffKey(machineId, project, staged),
    queryFn: () => api.gitDiff(machineId as string, project, staged),
    enabled: enabled && !!machineId && !!project,
    retry: (failureCount, error) => {
      if (error instanceof SessionExpiredError) return false;
      if (error instanceof ApiError) return false;
      return failureCount < 2;
    },
  });
}

// useGitBranch creates (and optionally checks out) a branch in a project (GR2).
// On success it invalidates the project's status/diff and the projects list so
// the new current branch shows everywhere it is displayed.
export function useGitBranch(machineId: string | null, project: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ name, checkout, from }: { name: string; checkout: boolean; from?: string }) =>
      api.gitBranch(machineId as string, project, name, checkout, from),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: gitStatusKey(machineId, project) });
      qc.invalidateQueries({ queryKey: gitDiffKey(machineId, project, false) });
      qc.invalidateQueries({ queryKey: gitDiffKey(machineId, project, true) });
      qc.invalidateQueries({ queryKey: ['projects'] });
    },
  });
}

// reconnectRequired reports whether an error is the GitHub "reconnect" signal.
export function reconnectRequired(error: unknown): boolean {
  return error instanceof ApiError && error.status === 409 && error.code === 'reconnect_github';
}

// useMachineEvents subscribes to the SSE stream. It writes live machine state
// into the machines-list cache (so useMachines stays current without polling)
// and keeps a rolling event log for display. The browser EventSource reconnects
// on its own; it replays Last-Event-ID automatically, and our server backfills
// the missed rows.
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
      // Replace the whole list — the snapshot is the authoritative current set.
      qc.setQueryData<MachineSummary[]>(machinesKey, data.machines);
      // Snapshot events arrive oldest-first; show newest-first.
      for (const ev of data.events) pushEvent(ev);
      log.debug('snapshot', { machines: data.machines.length, events: data.events.length });
    });

    es.addEventListener('machine', (e) => {
      const data = JSON.parse((e as MessageEvent).data) as MachineEventData;
      upsertMachine(qc, data.machine);
      pushEvent(data.event);
      log.debug('event', { type: data.event.type, to: data.event.to_state });
    });

    // A destroyed machine carries only its id; drop it from the list.
    es.addEventListener('destroyed', (e) => {
      const data = JSON.parse((e as MessageEvent).data) as MachineDestroyedData;
      removeMachine(qc, data.machine_id);
      log.debug('machine destroyed', { id: data.machine_id });
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
