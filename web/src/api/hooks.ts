import { useEffect, useRef, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  api,
  ApiError,
  machineEventsUrl,
  SessionExpiredError,
  taskEventsUrl,
  type CreateMachineInput,
  type CreateTaskInput,
  type MachineDestroyedData,
  type MachineEvent,
  type MachineEventData,
  type MachineSummary,
  type ProjectsResponse,
  type ReposResponse,
  type SnapshotData,
  type TaskEvent,
  type TasksResponse,
  type UserPrefs,
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

// useUpdateUserPrefs patches the user's account preferences and refreshes the
// cached /api/me so the Settings UI reflects the saved value.
export function useUpdateUserPrefs() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (prefs: Partial<UserPrefs>) => api.updateUserPrefs(prefs),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['me'] }),
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

// profileItemsKey is the query cache key for the user's portable-profile items.
const profileItemsKey = ['profile-items'] as const;

// useProfileItems loads the user's portable-profile items (Phase 1/2) — metadata
// only; the stored value is never returned. Drives the Claude-subscription panel's
// connected / needs-reconnect status.
export function useProfileItems() {
  return useQuery({
    queryKey: profileItemsKey,
    queryFn: api.listProfileItems,
    retry: (failureCount, error) => {
      if (error instanceof SessionExpiredError) return false;
      return failureCount < 2;
    },
  });
}

// useProfileMutations exposes set/delete of a profile item's write-only value.
// Both invalidate the items query so connection status re-renders from the server
// (the value itself is never held client-side). Connect/disconnect also re-inject
// to the user's running machines server-side, so the change takes effect without
// recreating a machine.
export function useProfileMutations() {
  const qc = useQueryClient();
  const invalidate = () => qc.invalidateQueries({ queryKey: profileItemsKey });

  const setItem = useMutation({
    mutationFn: ({ key, value }: { key: string; value: string }) => api.setProfileItem(key, value),
    onSuccess: invalidate,
  });
  const deleteItem = useMutation({
    mutationFn: (key: string) => api.deleteProfileItem(key),
    onSuccess: invalidate,
  });
  return { setItem, deleteItem };
}

// gitIdentityKey / sshKeyKey cache the Phase 4 typed profile conveniences.
const gitIdentityKey = ['git-identity'] as const;
const sshKeyKey = ['ssh-key'] as const;

// useGitIdentity loads the effective git identity (portable or GitHub default).
export function useGitIdentity() {
  return useQuery({
    queryKey: gitIdentityKey,
    queryFn: api.getGitIdentity,
    retry: (failureCount, error) => {
      if (error instanceof SessionExpiredError) return false;
      return failureCount < 2;
    },
  });
}

// useGitIdentityMutations exposes set/clear of the portable git identity. Both
// invalidate the query so the displayed identity + source re-render from the server.
export function useGitIdentityMutations() {
  const qc = useQueryClient();
  const invalidate = () => qc.invalidateQueries({ queryKey: gitIdentityKey });
  const set = useMutation({
    mutationFn: ({ name, email }: { name: string; email: string }) =>
      api.setGitIdentity(name, email),
    onSuccess: invalidate,
  });
  const clear = useMutation({ mutationFn: () => api.deleteGitIdentity(), onSuccess: invalidate });
  return { set, clear };
}

// useSSHKey loads the SSH key status (present + public key/fingerprint, never the
// private key).
export function useSSHKey() {
  return useQuery({
    queryKey: sshKeyKey,
    queryFn: api.getSSHKey,
    retry: (failureCount, error) => {
      if (error instanceof SessionExpiredError) return false;
      return failureCount < 2;
    },
  });
}

// useSSHKeyMutations exposes generate/delete of the SSH key. Both invalidate the
// query so status re-renders from the server. generate returns the public key in
// its result so the panel can show it immediately.
export function useSSHKeyMutations() {
  const qc = useQueryClient();
  const invalidate = () => qc.invalidateQueries({ queryKey: sshKeyKey });
  const generate = useMutation({ mutationFn: () => api.generateSSHKey(), onSuccess: invalidate });
  const remove = useMutation({ mutationFn: () => api.deleteSSHKey(), onSuccess: invalidate });
  return { generate, remove };
}

// tokensKey is the query cache key for the user's personal access tokens.
const tokensKey = ['tokens'] as const;

// useTokens loads the user's personal access tokens (AC1). The secret is never
// part of this data — only metadata + the non-secret prefix.
export function useTokens() {
  return useQuery({
    queryKey: tokensKey,
    queryFn: api.listTokens,
    retry: (failureCount, error) => {
      if (error instanceof SessionExpiredError) return false;
      return failureCount < 2;
    },
  });
}

// useTokenMutations exposes create/revoke of personal access tokens. Both
// invalidate the tokens query so the listing re-renders from the server. create
// returns the one-time plaintext to its caller (via the mutation result) so the
// UI can show it once; it is never cached.
export function useTokenMutations() {
  const qc = useQueryClient();
  const invalidate = () => qc.invalidateQueries({ queryKey: tokensKey });

  const create = useMutation({
    mutationFn: ({ name, expiresInDays }: { name: string; expiresInDays?: number }) =>
      api.createToken(name, expiresInDays),
    onSuccess: invalidate,
  });
  const revoke = useMutation({
    mutationFn: (id: string) => api.revokeToken(id),
    onSuccess: invalidate,
  });
  return { create, revoke };
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

// useGitCommit stages and commits changes in a project (GR3). On success it
// invalidates the project's status/diff (the tree goes clean) and the projects
// list (its last-commit updates).
export function useGitCommit(machineId: string | null, project: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ message, paths }: { message: string; paths?: string[] }) =>
      api.gitCommit(machineId as string, project, message, paths),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: gitStatusKey(machineId, project) });
      qc.invalidateQueries({ queryKey: gitDiffKey(machineId, project, false) });
      qc.invalidateQueries({ queryKey: gitDiffKey(machineId, project, true) });
      qc.invalidateQueries({ queryKey: ['projects'] });
    },
  });
}

// useGitPush dispatches a push of a branch to origin (GR4). It returns the op_id
// immediately (202); completion arrives as a git.push machine event over SSE, so
// there is nothing to invalidate here — the caller correlates by op_id.
export function useGitPush(machineId: string | null, project: string) {
  return useMutation({
    mutationFn: ({ branch, setUpstream }: { branch: string; setUpstream: boolean }) =>
      api.gitPush(machineId as string, project, branch, setUpstream),
  });
}

// useGitPR opens a pull request for the project (GR5). It returns the PR URL +
// number directly (a synchronous CP→GitHub call); the caller shows the link.
export function useGitPR(machineId: string | null, project: string) {
  return useMutation({
    mutationFn: ({ title, body, head }: { title: string; body: string; head: string }) =>
      api.gitPR(machineId as string, project, title, body, head),
  });
}

// PR review (mobile review loop): summary/files/checks keyed by repo full-name
// + number. 404/409/502 are ApiError — not retried, surfaced to the screen.
const prKey = (repo: string, number: number) => ['pr', repo, number] as const;

export function usePR(repo: string, number: number, enabled = true) {
  return useQuery({
    queryKey: prKey(repo, number),
    queryFn: () => api.getPR(repo, number),
    enabled: enabled && !!repo && number > 0,
    retry: (failureCount, error) => {
      if (error instanceof SessionExpiredError) return false;
      if (error instanceof ApiError) return false;
      return failureCount < 2;
    },
  });
}

export function usePRFiles(repo: string, number: number, enabled = true) {
  return useQuery({
    queryKey: [...prKey(repo, number), 'files'] as const,
    queryFn: () => api.getPRFiles(repo, number),
    enabled: enabled && !!repo && number > 0,
    retry: (failureCount, error) => {
      if (error instanceof SessionExpiredError) return false;
      if (error instanceof ApiError) return false;
      return failureCount < 2;
    },
  });
}

export function usePRChecks(repo: string, number: number, enabled = true) {
  return useQuery({
    queryKey: [...prKey(repo, number), 'checks'] as const,
    queryFn: () => api.getPRChecks(repo, number),
    enabled: enabled && !!repo && number > 0,
    retry: (failureCount, error) => {
      if (error instanceof SessionExpiredError) return false;
      if (error instanceof ApiError) return false;
      return failureCount < 2;
    },
  });
}

// useMergePR merges the PR. On success it invalidates the PR summary so the
// status chip flips to MERGED from the server's answer, not a local guess.
export function useMergePR(repo: string, number: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (method: 'merge' | 'squash' | 'rebase' = 'merge') =>
      api.mergePR(repo, number, method),
    onSuccess: () => qc.invalidateQueries({ queryKey: prKey(repo, number) }),
  });
}

// useCommentPR posts a plain PR comment (the mobile comment sheet).
export function useCommentPR(repo: string, number: number) {
  return useMutation({
    mutationFn: (body: string) => api.commentPR(repo, number, body),
  });
}

// tasksKey is the query cache key for a machine's agent tasks (keyed by machine
// so switching machines refetches the right set).
const tasksKey = (machineId: string | null) => ['tasks', machineId] as const;

// useTasks loads a machine's headless agent tasks (AT1/AT2), newest first. While
// the window is open it polls every few seconds so a task's status (and a new
// task created elsewhere) stays current without an extra event channel; the live
// per-task stream (useTaskEvents) is the high-resolution view.
export function useTasks(machineId: string | null, enabled: boolean) {
  return useQuery<TasksResponse>({
    queryKey: tasksKey(machineId),
    queryFn: () => api.listTasks(machineId as string),
    enabled: enabled && !!machineId,
    refetchInterval: 4000,
    retry: (failureCount, error) => {
      if (error instanceof SessionExpiredError) return false;
      if (error instanceof ApiError) return false;
      return failureCount < 2;
    },
  });
}

// useCreateTask dispatches a headless run; on success it invalidates the task
// list so the new (queued/running) row appears immediately.
export function useCreateTask(machineId: string | null) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: CreateTaskInput) => api.createTask(machineId as string, input),
    onSuccess: () => qc.invalidateQueries({ queryKey: tasksKey(machineId) }),
  });
}

// useCancelTask requests cancellation of a running task (AT3); on success it
// invalidates the list so the row reflects the pending stop (the terminal
// `canceled` status then arrives over the task SSE).
export function useCancelTask(machineId: string | null) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (taskId: string) => api.cancelTask(machineId as string, taskId),
    onSuccess: () => qc.invalidateQueries({ queryKey: tasksKey(machineId) }),
  });
}

// useTaskEvents subscribes to one task's live agent-event SSE stream (AT2),
// accumulating normalized events for display. The browser EventSource replays
// Last-Event-ID on its own reconnect; we close it on the terminal `result` frame
// so it does not loop. Switching the selected task resets the buffer. Bumping
// `epoch` forces a fresh reconnect — used to pick up a follow-up turn (AT4),
// whose server-side stream replays only the new turn.
export function useTaskEvents(
  machineId: string | null,
  taskId: string | null,
  epoch = 0,
): TaskEvent[] {
  const qc = useQueryClient();
  const [events, setEvents] = useState<TaskEvent[]>([]);

  useEffect(() => {
    setEvents([]);
    if (!machineId || !taskId) return;
    const es = new EventSource(taskEventsUrl(machineId, taskId), { withCredentials: true });

    es.addEventListener('agent', (e) => {
      const ev = JSON.parse((e as MessageEvent).data) as TaskEvent;
      setEvents((prev) => [...prev, ev]);
      if (ev.kind === 'result') {
        es.close(); // terminal — stop here rather than auto-reconnecting
        // The run ended; refresh the list so its row flips to done/failed.
        qc.invalidateQueries({ queryKey: tasksKey(machineId) });
      }
    });
    es.addEventListener('error', () => {
      log.debug('task stream error', { readyState: es.readyState });
    });

    return () => es.close();
  }, [machineId, taskId, epoch, qc]);

  return events;
}

// useSendMessage runs a follow-up turn on a finished task (AT4: resume). On
// success it invalidates the list so the row cycles back to running.
export function useSendMessage(machineId: string | null) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ taskId, prompt }: { taskId: string; prompt: string }) =>
      api.sendMessage(machineId as string, taskId, prompt),
    onSuccess: () => qc.invalidateQueries({ queryKey: tasksKey(machineId) }),
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
//
// onAuthLost is called when the stream closes and a /api/me probe confirms the
// session has expired — callers should redirect to /login.
export function useMachineEvents(onAuthLost?: () => void): MachineEvent[] {
  const qc = useQueryClient();
  const [events, setEvents] = useState<MachineEvent[]>([]);
  // Guard against duplicate ids across reconnect replays.
  const seen = useRef<Set<number>>(new Set());
  // Stable ref so the effect closure always calls the current callback without
  // needing onAuthLost in its dependency array (which would recreate the
  // EventSource on every render of the caller).
  const onAuthLostRef = useRef(onAuthLost);
  useEffect(() => { onAuthLostRef.current = onAuthLost; }, [onAuthLost]);
  // Timestamp of the last /api/me probe to avoid hammering it on a flapping stream.
  const lastAuthProbe = useRef(0);

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

    // EventSource auto-reconnects on transient errors. readyState CLOSED (2)
    // means the browser has given up — the most common cause mid-session is an
    // expired cookie. Probe /api/me to confirm; redirect to login on 401.
    es.addEventListener('error', () => {
      log.warn('stream error', { readyState: es.readyState });
      if (es.readyState === EventSource.CLOSED) {
        const now = Date.now();
        if (now - lastAuthProbe.current > 15_000) {
          lastAuthProbe.current = now;
          fetch('/api/me', {
            credentials: 'include',
            headers: { 'X-Requested-By': 'proteos-client' },
          })
            .then((r) => { if (r.status === 401) onAuthLostRef.current?.(); })
            .catch(() => {}); // network errors are not auth loss
        }
      }
    });

    return () => es.close();
  }, [qc]);

  return events;
}
