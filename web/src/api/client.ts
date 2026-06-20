// Typed fetch client for the control-plane API.
//
// Every request carries the X-Requested-By header (the CSRF defense that pairs
// with SameSite=Lax cookies), and a 401 is surfaced as a typed
// SessionExpiredError so the app can redirect to /login globally.

import { logger } from '../lib/logger';

const log = logger.child({ component: 'api' });

export class SessionExpiredError extends Error {
  constructor() {
    super('session expired');
    this.name = 'SessionExpiredError';
  }
}

export class ApiError extends Error {
  status: number;
  code: string;
  // detail is the optional human-readable elaboration some endpoints attach
  // (e.g. invalid_resources → "vcpus must be 1..8"). Undefined when absent.
  detail?: string;
  constructor(status: number, code: string, detail?: string) {
    super(`api error ${status}: ${code}`);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.detail = detail;
  }
}

export interface Me {
  user: {
    login: string;
    email: string;
    avatar_url: string;
  };
  // All of the user's machines (possibly empty), seeding the SPA's first paint.
  machines: MachineSummary[];
}

// MachineState mirrors the control-plane machines.state CHECK constraint.
export type MachineState =
  | 'requested'
  | 'provisioning'
  | 'running'
  | 'starting'
  | 'stopping'
  | 'hibernating'
  | 'stopped'
  | 'error';

// SnapshotSummary is the current hibernation snapshot metadata (Phase 4),
// present only while the machine is hibernated (stopped with a usable snapshot).
interface SnapshotSummary {
  fc_version: string;
  mem_bytes: number;
  created_at: string;
}

export interface MachineSummary {
  id: string;
  name: string;
  state: MachineState;
  guest_ip: string | null;
  kernel_ref: string;
  rootfs_ref: string;
  // The catalog template the machine was created from; null for legacy machines
  // created before templates existed.
  template_id: string | null;
  resource_spec: { vcpus: number; mem_mib: number; disk_mib?: number };
  last_error: string | null;
  created_at: string;

  // Phase 4: persistent disk + hibernate/resume.
  boot: 'cold' | 'resumed' | null;
  disk_id: string | null;
  disk_mib: number | null;
  snapshot: SnapshotSummary | null;
}

// A machine's resource spec: pinned vCPUs, memory, and disk (MiB). Reached
// through MachineTemplate; not exported on its own.
interface MachineResources {
  vcpus: number;
  mem_mib: number;
  disk_mib: number;
}

// Inclusive [min,max] range for one resource dimension.
interface ResourceBound {
  min: number;
  max: number;
}

interface ResourceLimits {
  vcpus: ResourceBound;
  mem_mib: ResourceBound;
  disk_mib: ResourceBound;
}

// MachineTemplate is one entry of GET /api/templates: a selectable machine image
// with default resources and the caps that bound a user's overrides. The rootfs/
// kernel refs are intentionally not exposed.
export interface MachineTemplate {
  id: string;
  label: string;
  description: string;
  defaults: MachineResources;
  limits: ResourceLimits;
}

// CreateMachineInput is the optional body of POST /api/machines. Every field is
// optional: empty name ⇒ auto-named; empty template_id ⇒ catalog default; unset
// resources ⇒ the chosen template's defaults.
export interface CreateMachineInput {
  name?: string;
  template_id?: string;
  vcpus?: number;
  mem_mib?: number;
  disk_mib?: number;
}

export interface MachineEvent {
  id: number;
  // "git.clone"/"git.push" carry an async git op completion (payload:
  // { op_id, ok, detail }); "git.pr" carries an opened PR (payload:
  // { number, url, project }).
  type: 'transition' | 'error' | 'info' | 'git.clone' | 'git.push' | 'git.pr';
  from_state: string | null;
  to_state: string | null;
  actor: string;
  payload: Record<string, unknown>;
  created_at: string;
}

// SSE payloads from GET /api/machine/events. The snapshot carries every machine
// the user owns; live `machine` events upsert one by id and `destroyed` removes one.
export interface SnapshotData {
  machines: MachineSummary[];
  events: MachineEvent[];
}
export interface MachineEventData {
  machine: MachineSummary;
  event: MachineEvent;
}
export interface MachineDestroyedData {
  machine_id: string;
}

// SecretField is one declared input a provider needs (Phase 6). The settings UI
// renders a form from these — name is the field key, label is the prompt, env is
// the variable it becomes inside the machine. None of these are secret.
interface SecretField {
  name: string;
  label: string;
  env: string;
}

// Provider is one row of GET /api/providers. The API never returns key material;
// key_set only reports whether the user has stored a key (Phase 5 decision #5).
// secret_fields drives the data-rendered settings form (Phase 6 decision #5).
export interface Provider {
  key: string;
  display_name: string;
  enabled: boolean;
  key_set: boolean;
  secret_fields: SecretField[];
}

// Repo is one row of GET /api/git/repos — a repository the user has granted the
// GitHub App access to (Phase 7). pushed_at is RFC3339.
export interface Repo {
  full_name: string;
  private: boolean;
  default_branch: string;
  pushed_at: string;
}

// ReposResponse is GET /api/git/repos. grants_url links to the App's
// installation-settings page so the user can choose which repos ProteOS can see;
// it may be "" when the App slug is unconfigured.
export interface ReposResponse {
  repos: Repo[];
  grants_url: string;
}

// CloneStarted is the 202 body of POST /api/git/clone; completion arrives as a
// git.clone machine event over the SSE stream.
export interface CloneStarted {
  op_id: string;
}

// WebSession is the 200 body of POST /api/machine/web-session (Phase 8): a
// one-shot, ≤60s URL on the machine's editor subdomain. The SPA navigates the
// editor iframe (or a new tab) to it; the machine origin validates the token and
// sets its partitioned cookie, then 302s to the editor root.
export interface WebSession {
  url: string;
}

// Project is one cloned repository under /workspace (Phase 9), as returned by
// GET /api/projects. The filesystem is the source of truth; the list is fetched
// live and refetched on the git.clone SSE event.
export interface Project {
  name: string;
  path: string;
  remote?: string;
  branch?: string;
  dirty: boolean;
  last_commit_at?: string;
  last_commit_msg?: string;
}

export interface ProjectsResponse {
  projects: Project[];
}

// GitFileStatus is one changed path in GET /api/machines/{id}/git/status (GR1).
// index/worktree are single-character porcelain codes: the staged (index-vs-HEAD)
// and unstaged (worktree-vs-index) states — e.g. "M" modified, "A" added, "D"
// deleted, "R" renamed, "?" untracked (both fields), " " unchanged in that area.
// orig is set only for renames/copies (the path the change came from).
export interface GitFileStatus {
  path: string;
  orig?: string;
  index: string;
  worktree: string;
}

// GitStatusResponse is GET /api/machines/{id}/git/status: the current branch and
// the working-tree change set (empty files ⇒ a clean tree).
export interface GitStatusResponse {
  branch?: string;
  files: GitFileStatus[];
}

// GitDiffResponse is GET /api/machines/{id}/git/diff: a unified diff, capped on
// the guest. truncated ⇒ the diff exceeded the cap and was cut (inspect the rest
// in the terminal/editor). Tracked changes only — untracked files show in status.
export interface GitDiffResponse {
  diff: string;
  truncated: boolean;
}

// GitBranchResponse is POST /api/machines/{id}/git/branch (GR2): the current
// branch after the op (the new branch when checkout was requested).
export interface GitBranchResponse {
  branch: string;
}

// GitCommitResponse is POST /api/machines/{id}/git/commit (GR3): the new HEAD
// short sha and its subject line.
export interface GitCommitResponse {
  sha: string;
  subject: string;
}

// PushStarted is the 202 body of POST /api/machines/{id}/git/push (GR4): the op
// id to correlate with the later git.push machine event over SSE.
export interface PushStarted {
  op_id: string;
}

// PRCreated is the 200 body of POST /api/machines/{id}/git/pr (GR5): the opened
// pull request's URL and number.
export interface PRCreated {
  pr_url: string;
  number: number;
}

// DesktopLayout is the opaque serialized window layout stored in machine SQLite
// (Phase 9 decision #6). The control plane relays it verbatim; only the desktop
// understands its shape. null ⇒ no layout saved yet.
export interface DesktopResponse {
  layout: unknown | null;
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const method = init.method ?? 'GET';
  let res: Response;
  try {
    res = await fetch(path, {
      ...init,
      headers: {
        'X-Requested-By': 'proteos',
        ...(init.headers ?? {}),
      },
      credentials: 'same-origin',
    });
  } catch (err) {
    // Network failure (offline, DNS, CORS) — fetch rejects without a Response.
    log.error('request failed', { method, path, err: err as Error });
    throw err;
  }

  if (res.status === 401) {
    // Routine "not signed in" signal, not an error; logged at debug.
    log.debug('session expired', { method, path });
    throw new SessionExpiredError();
  }
  if (!res.ok) {
    let code = 'error';
    let detail: string | undefined;
    try {
      const body = (await res.json()) as { error?: string; detail?: string };
      if (body.error) code = body.error;
      if (body.detail) detail = body.detail;
    } catch {
      // non-JSON error body; keep the generic code
    }
    log.warn('request error', { method, path, status: res.status, code });
    throw new ApiError(res.status, code, detail);
  }
  // 204 / empty body tolerance.
  const text = await res.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

export const api = {
  me: () => request<Me>('/api/me'),
  logout: () => request<void>('/api/auth/logout', { method: 'POST' }),

  // GET /api/templates returns the machine-template catalog for the create
  // picker (possibly empty on a legacy single-image deployment).
  getTemplates: () => request<MachineTemplate[]>('/api/templates'),

  // GET /api/machines returns all of the user's machines (possibly empty).
  listMachines: () => request<MachineSummary[]>('/api/machines'),
  // POST /api/machines provisions a new machine. The body is optional (empty ⇒
  // auto-named, catalog default, default resources). 409 machine_limit, 400
  // unknown_template / invalid_resources (ApiError; the latter carries a detail).
  createMachine: (input: CreateMachineInput = {}) => {
    const hasBody = Object.values(input).some((v) => v !== undefined && v !== '');
    return request<MachineSummary>('/api/machines', {
      method: 'POST',
      headers: hasBody ? { 'Content-Type': 'application/json' } : {},
      body: hasBody ? JSON.stringify(input) : undefined,
    });
  },
  startMachine: (id: string) =>
    request<MachineSummary>(`/api/machines/${encodeURIComponent(id)}/start`, { method: 'POST' }),
  stopMachine: (id: string) =>
    request<MachineSummary>(`/api/machines/${encodeURIComponent(id)}/stop`, { method: 'POST' }),
  destroyMachine: (id: string) =>
    request<void>(`/api/machines/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  renameMachine: (id: string, name: string) =>
    request<MachineSummary>(`/api/machines/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name }),
    }),
  // Mint a one-shot editor URL for a running machine (Phase 8). 409
  // machine_not_running / 404 no_machine surface as ApiError. Only available when
  // the control plane has PROTEOS_MACHINE_DOMAIN set; otherwise the route 404s.
  // An optional `folder` opens code-server directly on a project (Phase 9 #5);
  // 400 bad_folder if it is not a listable project.
  webSession: (machineID: string, folder?: string) =>
    request<WebSession>(`/api/machine/web-session?machine=${encodeURIComponent(machineID)}`, {
      method: 'POST',
      headers: folder ? { 'Content-Type': 'application/json' } : {},
      body: folder ? JSON.stringify({ folder }) : undefined,
    }),

  // Mint a one-shot preview URL for a running machine's app on `port` (PP3): the
  // same endpoint with ?port=, returning a URL on the m-<uuid>-p<port> origin.
  // 400 bad_request if the port is reserved or outside the configured range; 409
  // machine_not_running / 404 no_machine as ApiError. The token is single-use, so
  // each consumer (the iframe, an "open in new tab") mints its own.
  previewSession: (machineID: string, port: number) =>
    request<WebSession>(
      `/api/machine/web-session?machine=${encodeURIComponent(machineID)}&port=${port}`,
      { method: 'POST' },
    ),

  // Projects + desktop layout (Phase 9), scoped to a specific machine via
  // ?machine=. listProjects 409s when the machine is not running. getDesktop/
  // putDesktop relay the opaque layout to/from that machine's SQLite; putDesktop
  // is a 204 (a no-op on a diskless stack).
  listProjects: (machineID: string) =>
    request<ProjectsResponse>(`/api/projects?machine=${encodeURIComponent(machineID)}`),
  getDesktop: (machineID: string) =>
    request<DesktopResponse>(`/api/machine/desktop?machine=${encodeURIComponent(machineID)}`),
  putDesktop: (machineID: string, layout: unknown) =>
    request<void>(`/api/machine/desktop?machine=${encodeURIComponent(machineID)}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ layout }),
    }),

  // Providers + write-only secret keys. setProviderKey/deleteProviderKey return
  // 204; values are never echoed back. fields maps each declared secret field
  // name to its value (Phase 6 generalizes Phase 5's single api_key body).
  listProviders: () => request<Provider[]>('/api/providers'),
  setProviderKey: (key: string, fields: Record<string, string>) =>
    request<void>(`/api/secrets/providers/${encodeURIComponent(key)}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ fields }),
    }),
  deleteProviderKey: (key: string) =>
    request<void>(`/api/secrets/providers/${encodeURIComponent(key)}`, { method: 'DELETE' }),

  // Git operations (Phase 7). listRepos may throw ApiError 409 reconnect_github
  // when the GitHub grant is revoked; cloneRepo returns an op_id and the clone
  // completes asynchronously (watch git.clone machine events).
  listRepos: () => request<ReposResponse>('/api/git/repos'),
  cloneRepo: (machineID: string, fullName: string) =>
    request<CloneStarted>(`/api/git/clone?machine=${encodeURIComponent(machineID)}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ full_name: fullName }),
    }),

  // Worktree review (GR1): read a project's git status / unified diff. project is
  // the repo's /workspace directory name. 400 bad_project if it is not a listable
  // project; 409 machine_not_running; 502 guest_unreachable (all ApiError).
  gitStatus: (machineID: string, project: string) =>
    request<GitStatusResponse>(
      `/api/machines/${encodeURIComponent(machineID)}/git/status?project=${encodeURIComponent(project)}`,
    ),
  gitDiff: (machineID: string, project: string, staged: boolean) =>
    request<GitDiffResponse>(
      `/api/machines/${encodeURIComponent(machineID)}/git/diff?project=${encodeURIComponent(project)}&staged=${staged ? 'true' : 'false'}`,
    ),

  // Create (and optionally check out) a branch in a project (GR2). 400
  // invalid_branch_name / bad_request; 409 branch_exists / machine_not_running;
  // 422 branch_failed (e.g. a bad start point) — all ApiError.
  gitBranch: (machineID: string, project: string, name: string, checkout: boolean, from?: string) =>
    request<GitBranchResponse>(`/api/machines/${encodeURIComponent(machineID)}/git/branch`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ project, name, checkout, from: from || undefined }),
    }),

  // Stage (the given paths, or all changes) and commit them in a project (GR3).
  // 400 empty_message / bad_request; 409 nothing_to_commit / machine_not_running;
  // 422 commit_failed — all ApiError. Omit paths to commit everything.
  gitCommit: (machineID: string, project: string, message: string, paths?: string[]) =>
    request<GitCommitResponse>(`/api/machines/${encodeURIComponent(machineID)}/git/commit`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ project, message, paths: paths && paths.length ? paths : undefined }),
    }),

  // Push a branch to origin (GR4). Returns 202 + op_id immediately; completion
  // arrives as a git.push machine event over SSE. 400 invalid_branch_name /
  // bad_request; 409 machine_not_running — all ApiError.
  gitPush: (machineID: string, project: string, branch: string, setUpstream: boolean) =>
    request<PushStarted>(`/api/machines/${encodeURIComponent(machineID)}/git/push`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ project, branch, set_upstream: setUpstream }),
    }),

  // Open a pull request from head into the repo's default branch (GR5). 400
  // bad_request / invalid_branch_name; 409 pr_exists / reconnect_github /
  // machine_not_running; 422 no_commits / no_remote / bad_remote; 502
  // github_unavailable — all ApiError.
  gitPR: (machineID: string, project: string, title: string, body: string, head: string) =>
    request<PRCreated>(`/api/machines/${encodeURIComponent(machineID)}/git/pr`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ project, title, body, head }),
    }),
};

// SSE endpoint for live machine state; consumed by useMachineEvents via the
// browser EventSource API (cookie auth, no custom headers).
export const machineEventsUrl = '/api/machine/events';

// The login redirect is a full navigation (not fetch) so the browser follows
// GitHub's 302 chain and cookies are set on the top-level document.
export const loginUrl = '/api/auth/github/login';
