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

// UserPrefs are the user's account-level preferences. download_as_is selects
// what the project Download button includes: true ⇒ the full folder as-is
// (.git + ignored files); false (default) ⇒ a clean export. claude_attribution
// selects whether Claude Code stamps its attribution on commits/PRs: true
// (default) keeps Claude's own defaults; false blanks them on the user's
// machines.
export interface UserPrefs {
  download_as_is: boolean;
  claude_attribution: boolean;
}

export interface Me {
  user: {
    login: string;
    email: string;
    avatar_url: string;
  };
  prefs: UserPrefs;
  // All of the user's machines (possibly empty), seeding the SPA's first paint.
  machines: MachineSummary[];
  // The per-user machine cap (global, deployment-configured; currently 5).
  machine_limit: number;
  // False until the user completes Connect GitHub (TAV-149); the SPA blocks on
  // the connect screen until then, since git operations need the linked account.
  github_connected: boolean;
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
  last_active_at: string | null;

  // Phase 4: persistent disk + hibernate/resume.
  boot: 'cold' | 'resumed' | null;
  disk_id: string | null;
  disk_mib: number | null;
  snapshot: SnapshotSummary | null;
}

// Per-machine outcome of a DELETE /api/machines bulk destroy call.
export interface DestroyAllResult {
  id: string;
  name: string;
  ok: boolean;
  error?: string;
  // export_failed (TAV-141) is true when this machine's failure was a
  // blocked session export specifically, distinct from other destroy
  // failures — the UI uses it to offer a "force delete anyway" retry.
  export_failed?: boolean;
}

// Summary returned by DELETE /api/machines.
export interface DestroyAllResponse {
  total: number;
  destroyed: number;
  failed: number;
  results: DestroyAllResult[];
}

// Per-machine outcome of a POST /api/machines/fill bulk create call. id/name
// are omitted for a failed create (ok: false).
export interface CreateAllResult {
  id?: string;
  name?: string;
  ok: boolean;
  error?: string;
}

// Summary returned by POST /api/machines/fill.
export interface CreateAllResponse {
  requested: number;
  created: number;
  failed: number;
  results: CreateAllResult[];
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

// NetworkPolicyMode mirrors the control-plane network_policies.mode CHECK
// constraint (TAV-116). allow_all is the default for a machine with no policy
// row (see NetworkPolicy).
export type NetworkPolicyMode = 'allow_all' | 'deny_all' | 'allow_domains' | 'deny_domains';

// NetworkPolicy is the body of GET/PUT /api/machines/{id}/network-policy: a
// machine's egress/ingress configuration. domains is meaningful only for the
// two domain-list modes; it is always present (possibly empty).
export interface NetworkPolicy {
  mode: NetworkPolicyMode;
  domains: string[];
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

// ProfileItem is one row of GET /api/profile/items — a portable user-profile
// item (Phase 1/2), e.g. the Claude subscription token. The API never returns the
// stored value; `connected` reports that a value is set and `needs_reconnect`
// that it is known-expired (from metadata). Timestamps are RFC3339; expires_at is
// omitted when the item has no expiry.
export interface ProfileItem {
  key: string;
  kind: string;
  target: string;
  connected: boolean;
  needs_reconnect: boolean;
  created_at: string;
  updated_at: string;
  expires_at?: string;
}

// The well-known profile item key for the Claude subscription OAuth token
// (`claude setup-token` output → CLAUDE_CODE_OAUTH_TOKEN in the machine).
export const CLAUDE_OAUTH_KEY = 'claude-oauth';

// GitIdentity is GET /api/profile/git: the effective git identity written to
// ~/.gitconfig on the user's machines. source is "profile" when the user set a
// portable identity, else "github" (the GitHub-derived default).
export interface GitIdentity {
  name: string;
  email: string;
  source: 'profile' | 'github';
}

// SSHKeyStatus is GET/POST /api/profile/ssh. The private key is never returned;
// only the public key (to add to GitHub) and its fingerprint. public_key/
// fingerprint are present only when present is true.
export interface SSHKeyStatus {
  present: boolean;
  public_key?: string;
  fingerprint?: string;
}

// AccessToken is one row of GET /api/tokens (AC1) — a personal access token the
// user minted for the CLI. The secret is never returned here; only the non-secret
// `prefix` (the token's leading characters) is shown so tokens are distinguishable.
// Timestamps are RFC3339; expires_at/last_used_at are omitted when null.
export interface AccessToken {
  id: string;
  name: string;
  prefix: string;
  created_at: string;
  last_used_at?: string;
  expires_at?: string;
}

// CreatedToken is the POST /api/tokens response — the ONLY time the plaintext
// `token` is returned. It cannot be recovered later.
export interface CreatedToken {
  id: string;
  name: string;
  token: string;
  prefix: string;
  expires_at?: string;
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

// CloneTarget is the body of POST /api/git/clone: exactly one of full_name (a
// GitHub repo, cloned from the server's auth host) or url (clone-by-URL — the
// host must be on the server's public-host allowlist).
export type CloneTarget = { full_name: string; url?: never } | { url: string; full_name?: never };

// GitHost is one row of GET /api/git/hosts (Gitea/Forgejo phase 2): an
// operator-allowlisted additional git host and whether the user has a PAT
// saved for it. The token is write-only — only the host login is readable.
export interface GitHost {
  host: string;
  linked: boolean;
  login?: string;
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

// PR review (mobile review loop): summary/files/checks/merge/comment for a PR,
// keyed by repo full-name + number. All CP→GitHub calls — machine-independent,
// so a PR stays reviewable while its machine is stopped.

// PRState is the review-surface state the OPEN/DRAFT/MERGED/CLOSED chip renders.
export type PRState = 'open' | 'draft' | 'merged' | 'closed';

// PRDetail is GET /api/git/repos/{owner}/{repo}/pulls/{number}.
export interface PRDetail {
  number: number;
  state: PRState;
  title: string;
  body?: string;
  html_url: string;
  head: string;
  base: string;
  head_sha: string;
  author: { login: string; avatar_url: string };
  additions: number;
  deletions: number;
  changed_files: number;
}

// PRFileStatus is the single-letter file status the review list renders.
export type PRFileStatus = 'A' | 'M' | 'D' | 'R';

// PRFile is one row of GET .../pulls/{number}/files. patch is absent for binary
// or oversized files (GitHub omits it).
export interface PRFile {
  path: string;
  prev_path?: string;
  status: PRFileStatus;
  additions: number;
  deletions: number;
  patch?: string;
}

export interface PRFilesResponse {
  files: PRFile[];
}

// PRChecks is GET .../pulls/{number}/checks: the head commit's check runs plus
// the counts the stat strip renders.
export interface PRChecks {
  total: number;
  passed: number;
  failed: number;
  pending: number;
  runs: { name: string; status: string; conclusion?: string }[];
}

// PRMergeResult is the 200 body of POST .../pulls/{number}/merge.
export interface PRMergeResult {
  merged: boolean;
  sha: string;
}

// PRCommentResult is the 200 body of POST .../pulls/{number}/comments.
export interface PRCommentResult {
  id: number;
  html_url: string;
}

// prPath builds the /api/git/repos/{owner}/{repo}/pulls/{number} prefix from a
// repo full-name ("owner/name"), encoding each path segment.
function prPath(repo: string, number: number): string {
  const [owner, name] = repo.split('/');
  return `/api/git/repos/${encodeURIComponent(owner ?? '')}/${encodeURIComponent(name ?? '')}/pulls/${number}`;
}

// AgentTask is one row of the headless task lane (AT1/AT2). status moves
// queued → running → done | failed | canceled; terminal states are immutable.
// The result fields populate once the run ends. usage carries the agent's
// reported cost/turns/duration. The run leaves a dirty working tree — committing
// is the separate, explicit git-review flow.
export interface AgentTask {
  id: string;
  status: 'queued' | 'running' | 'done' | 'failed' | 'canceled';
  provider: string;
  project: string;
  prompt: string;
  agent_session_id?: string;
  usage?: Record<string, unknown>;
  result_summary?: string;
  error?: string;
  created_at: string;
  started_at?: string;
  ended_at?: string;
}

export interface TasksResponse {
  tasks: AgentTask[];
}

// TaskCreated is the 202 body of POST /api/machines/{id}/tasks.
export interface TaskCreated {
  task_id: string;
}

export interface CreateTaskInput {
  prompt: string;
  provider: string;
  project: string;
}

// TaskEvent is one normalized frame from the task SSE stream (AT2). kind
// discriminates the shape: assistant_text (prose), tool_use (a tool call),
// tool_result (its output), and the terminal result. Payloads are normalized on
// the guest and carry no secrets.
export interface TaskEvent {
  kind: 'assistant_text' | 'tool_use' | 'tool_result' | 'result';
  text?: string;
  tool?: string;
  tool_id?: string;
  input?: unknown;
  output?: string;
  is_error?: boolean;
  // result-only fields:
  status?: string;
  cost_usd?: number;
  num_turns?: number;
  duration_ms?: number;
  error?: string;
}

// DesktopLayout is the opaque serialized window layout stored in machine SQLite
// (Phase 9 decision #6). The control plane relays it verbatim; only the desktop
// understands its shape. null ⇒ no layout saved yet.
export interface DesktopResponse {
  layout: unknown | null;
}

// LogSource distinguishes the control plane's own request/lifecycle logs
// ("api") from warn/error lines reported by browser sessions ("ui"). Never
// includes Firecracker/guest logs — those are a separate per-machine concern.
export type LogSource = 'api' | 'ui';

// LogEntry is one line of GET /api/logs (TAV-108).
export interface LogEntry {
  time: string;
  level: string;
  source: LogSource;
  message: string;
  fields?: Record<string, string>;
}

export interface LogsResponse {
  entries: LogEntry[];
}

// SessionStatusFilter narrows GET /api/sessions to in-progress ("active":
// queued/running) or terminal ("finished": done/failed/canceled) coding agent
// sessions; "all" applies no filter (TAV-107).
export type SessionStatusFilter = 'all' | 'active' | 'finished';

// AgentSession is one coding agent session (an agent_tasks row) in the GET
// /api/sessions responses — the same headless task lane as AgentTask, but
// across every machine the caller owns and tagged with where it ran, for the
// Sessions page (TAV-107).
export interface AgentSession {
  id: string;
  machine_id: string;
  machine_name: string;
  status: 'queued' | 'running' | 'done' | 'failed' | 'canceled';
  provider: string;
  project: string;
  prompt: string;
  agent_session_id?: string;
  usage?: Record<string, unknown>;
  result_summary?: string;
  error?: string;
  created_at: string;
  started_at?: string;
  ended_at?: string;
}

export interface SessionsResponse {
  sessions: AgentSession[];
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
  // PATCH /api/user/preferences applies a partial update to the user's account
  // preferences and returns the full updated set. request() adds the CSRF header.
  updateUserPrefs: (prefs: Partial<UserPrefs>) =>
    request<UserPrefs>('/api/user/preferences', {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(prefs),
    }),
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
  // force (TAV-141) bypasses a blocked session export and deletes the
  // machine regardless of the export outcome.
  destroyMachine: (id: string, force?: boolean) =>
    request<void>(`/api/machines/${encodeURIComponent(id)}${force ? '?force=true' : ''}`, {
      method: 'DELETE',
    }),
  // DELETE /api/machines destroys every machine the user owns. Always resolves
  // with a per-machine breakdown even if some machines fail — it never rejects
  // on a partial failure (only on network/auth errors). force (TAV-141) is
  // forwarded to every machine's destroy, bypassing a blocked session export.
  destroyAllMachines: (force?: boolean) =>
    request<DestroyAllResponse>(`/api/machines${force ? '?force=true' : ''}`, {
      method: 'DELETE',
    }),
  // POST /api/machines/fill creates machines (default template/specs) until the
  // account reaches its machine_limit. Always resolves with a per-machine
  // breakdown even if some creates fail — it never rejects on a partial
  // failure (only on network/auth errors). Requested: 0 when already at limit.
  createUpToLimit: () => request<CreateAllResponse>('/api/machines/fill', { method: 'POST' }),
  renameMachine: (id: string, name: string) =>
    request<MachineSummary>(`/api/machines/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name }),
    }),
  // Network policy (TAV-116): getNetworkPolicy defaults to allow_all for a
  // machine with no policy configured. setNetworkPolicy takes effect on the
  // machine's next (re)boot, not immediately on an already-running machine.
  getNetworkPolicy: (id: string) =>
    request<NetworkPolicy>(`/api/machines/${encodeURIComponent(id)}/network-policy`),
  setNetworkPolicy: (id: string, policy: NetworkPolicy) =>
    request<NetworkPolicy>(`/api/machines/${encodeURIComponent(id)}/network-policy`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(policy),
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
  // projectDownloadUrl is the GET URL that streams a project's current contents
  // as a zip. The response is Content-Disposition: attachment, so navigating to
  // it (an <a download> click) downloads without leaving the desktop; cookie
  // auth on the same origin authorizes it. Not a request() call — the body is a
  // binary stream, not JSON.
  projectDownloadUrl: (machineID: string, projectPath: string) =>
    `/api/projects/download?machine=${encodeURIComponent(machineID)}&path=${encodeURIComponent(projectPath)}`,
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

  // Portable user profile (Phase 1/2). listProfileItems returns metadata only
  // (never the stored value); setProfileItem/deleteProfileItem return 204. The
  // server fixes each item's kind/target by key, so the body is just the value.
  // 404 unknown_item for an unregistered key; 422 missing_value / value_too_long.
  listProfileItems: () => request<ProfileItem[]>('/api/profile/items'),
  setProfileItem: (key: string, value: string) =>
    request<void>(`/api/profile/items/${encodeURIComponent(key)}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ value }),
    }),
  deleteProfileItem: (key: string) =>
    request<void>(`/api/profile/items/${encodeURIComponent(key)}`, { method: 'DELETE' }),

  // Git identity (Phase 4): the portable name/email written to ~/.gitconfig. set
  // returns 204; delete reverts to the GitHub default (404 when none was set).
  getGitIdentity: () => request<GitIdentity>('/api/profile/git'),
  setGitIdentity: (name: string, email: string) =>
    request<void>('/api/profile/git', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name, email }),
    }),
  deleteGitIdentity: () => request<void>('/api/profile/git', { method: 'DELETE' }),

  // SSH key (Phase 4): generate replaces any existing key and returns the public
  // key once for the user to add to GitHub; the private key never leaves the
  // server. delete removes it (404 when none).
  getSSHKey: () => request<SSHKeyStatus>('/api/profile/ssh'),
  generateSSHKey: () => request<SSHKeyStatus>('/api/profile/ssh', { method: 'POST' }),
  deleteSSHKey: () => request<void>('/api/profile/ssh', { method: 'DELETE' }),

  // Personal access tokens (AC1): the user mints/revokes CLI credentials here.
  // createToken returns the plaintext exactly once (shown then discarded);
  // listTokens never returns it. expiresInDays omitted / 0 ⇒ never expires.
  listTokens: () => request<{ tokens: AccessToken[] }>('/api/tokens').then((r) => r.tokens),
  createToken: (name: string, expiresInDays?: number) =>
    request<CreatedToken>('/api/tokens', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name, expires_in_days: expiresInDays ?? 0 }),
    }),
  revokeToken: (id: string) =>
    request<void>(`/api/tokens/${encodeURIComponent(id)}`, { method: 'DELETE' }),

  // Git operations (Phase 7). listRepos may throw ApiError 409 reconnect_github
  // when the GitHub grant is revoked; cloneRepo returns an op_id and the clone
  // completes asynchronously (watch git.clone machine events). The target is
  // either { full_name } (GitHub) or { url } (clone-by-URL — 400 forbidden_host
  // when the host is not on the server's allowlist, 400 bad_url when malformed).
  listRepos: () => request<ReposResponse>('/api/git/repos'),
  cloneRepo: (machineID: string, target: CloneTarget) =>
    request<CloneStarted>(`/api/git/clone?machine=${encodeURIComponent(machineID)}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(target),
    }),

  // Git-host PATs (Gitea/Forgejo phase 2). The token is validated against the
  // host then stored — never returned. setGitHostToken throws ApiError 400
  // bad_token when the host rejects it, 404 unknown_host when the host is not
  // allowlisted, 502 githost_unavailable when unreachable.
  listGitHosts: () => request<{ hosts: GitHost[] }>('/api/git/hosts').then((r) => r.hosts),
  setGitHostToken: (host: string, token: string) =>
    request<GitHost>(`/api/git/hosts/${encodeURIComponent(host)}/token`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token }),
    }),
  deleteGitHostToken: (host: string) =>
    request<void>(`/api/git/hosts/${encodeURIComponent(host)}/token`, { method: 'DELETE' }),

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

  // PR review reads. 404 no_pr (unknown repo/PR or no access — uniform); 400
  // bad_full_name / bad_number; 409 reconnect_github; 502 github_unavailable —
  // all ApiError. repo is the "owner/name" full-name from the deep link.
  getPR: (repo: string, number: number) => request<PRDetail>(prPath(repo, number)),
  getPRFiles: (repo: string, number: number) =>
    request<PRFilesResponse>(`${prPath(repo, number)}/files`),
  getPRChecks: (repo: string, number: number) =>
    request<PRChecks>(`${prPath(repo, number)}/checks`),
  // Merge the PR — the review surface's one primary action. 422 not_mergeable
  // (draft/conflicts/branch protection); 409 head_changed / reconnect_github;
  // 403 merge_forbidden — all ApiError.
  mergePR: (repo: string, number: number, method: 'merge' | 'squash' | 'rebase' = 'merge') =>
    request<PRMergeResult>(`${prPath(repo, number)}/merge`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ method }),
    }),
  // Post a plain PR comment (the mobile comment sheet). 400 empty_comment.
  commentPR: (repo: string, number: number, body: string) =>
    request<PRCommentResult>(`${prPath(repo, number)}/comments`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ body }),
    }),

  // Headless agent tasks (AT1/AT2). createTask dispatches a run and returns 202 +
  // task_id immediately; the run streams structured events over the task SSE and
  // ends leaving a dirty working tree. 400 provider_not_headless / bad_project;
  // 409 no_provider_key / machine_not_running — all ApiError. listTasks/getTask
  // are reads (getTask works even on a stopped machine).
  listTasks: (machineID: string) =>
    request<TasksResponse>(`/api/machines/${encodeURIComponent(machineID)}/tasks`),
  getTask: (machineID: string, taskID: string) =>
    request<AgentTask>(
      `/api/machines/${encodeURIComponent(machineID)}/tasks/${encodeURIComponent(taskID)}`,
    ),
  createTask: (machineID: string, input: CreateTaskInput) =>
    request<TaskCreated>(`/api/machines/${encodeURIComponent(machineID)}/tasks`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(input),
    }),
  // Cancel a running task (AT3). 202 dispatches the cancel; the terminal
  // `canceled` status arrives over the task SSE. 200 is an idempotent no-op when
  // the task already finished; 409 machine_not_running — all ApiError otherwise.
  cancelTask: (machineID: string, taskID: string) =>
    request<TaskCreated | AgentTask>(
      `/api/machines/${encodeURIComponent(machineID)}/tasks/${encodeURIComponent(taskID)}/cancel`,
      { method: 'POST' },
    ),
  // Send a follow-up turn on a finished task (AT4): resumes the agent session
  // with a new prompt; the new turn streams over the same task SSE. 409
  // no_session (never captured) / task_running (a turn is in flight); 400
  // provider_not_headless; 409 no_provider_key / machine_not_running — all
  // ApiError otherwise.
  sendMessage: (machineID: string, taskID: string, prompt: string) =>
    request<TaskCreated>(
      `/api/machines/${encodeURIComponent(machineID)}/tasks/${encodeURIComponent(taskID)}/messages`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ prompt }),
      },
    ),

  // Proteos application logs (TAV-108): the control plane's own logs plus
  // browser-reported UI errors — never Firecracker/machine logs. source omitted
  // ⇒ both; limit caps the count (server default 500, max 2000).
  listLogs: (source?: LogSource, limit?: number) => {
    const params = new URLSearchParams();
    if (source) params.set('source', source);
    if (limit) params.set('limit', String(limit));
    const qs = params.toString();
    return request<LogsResponse>(`/api/logs${qs ? `?${qs}` : ''}`);
  },
  // Reports one browser-side log record so it shows up alongside the server's
  // own logs. Best-effort — see lib/uiLogReporter.ts, which is the only caller.
  reportUILog: (level: string, message: string, fields?: Record<string, string>) =>
    request<void>('/api/logs/ui', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ level, message, fields }),
    }),

  // Coding agent sessions (TAV-107): the caller's agent_tasks rows across every
  // machine they own, newest first. status omitted ⇒ both active and finished;
  // limit caps the count (server default 500, max 2000).
  listSessions: (status?: SessionStatusFilter, limit?: number) => {
    const params = new URLSearchParams();
    if (status && status !== 'all') params.set('status', status);
    if (limit) params.set('limit', String(limit));
    const qs = params.toString();
    return request<SessionsResponse>(`/api/sessions${qs ? `?${qs}` : ''}`);
  },
  // One session by id, full detail (TAV-142). 404 no_session if it does not
  // exist or belongs to another user.
  getSession: (id: string) => request<AgentSession>(`/api/sessions/${encodeURIComponent(id)}`),
};

// logsExportUrl is the GET URL that downloads captured logs as a plain-text
// attachment (TAV-108's Export button). Not a request() call — the body is a
// text stream, not JSON; same pattern as projectDownloadUrl.
export function logsExportUrl(source?: LogSource): string {
  return `/api/logs/export${source ? `?source=${encodeURIComponent(source)}` : ''}`;
}

// sessionsExportUrl is the GET URL that downloads the caller's coding agent
// sessions as a CSV attachment (TAV-107's Export button). Not a request()
// call — the body is a CSV stream, not JSON; same pattern as logsExportUrl.
export function sessionsExportUrl(status?: SessionStatusFilter): string {
  return `/api/sessions/export${status && status !== 'all' ? `?status=${encodeURIComponent(status)}` : ''}`;
}

// sessionExportUrl is the GET URL that downloads one session as a JSON
// attachment (TAV-142 detail view's Export button). Not a request() call —
// same <a download> pattern as sessionsExportUrl/logsExportUrl.
export function sessionExportUrl(id: string): string {
  return `/api/sessions/${encodeURIComponent(id)}/export`;
}

// taskExportUrl is the GET URL that downloads one task as a JSON attachment
// (the task detail view's Export button). Not a request() call — same
// <a download> pattern as sessionExportUrl.
export function taskExportUrl(machineID: string, taskID: string): string {
  return `/api/machines/${encodeURIComponent(machineID)}/tasks/${encodeURIComponent(taskID)}/export`;
}

// taskEventsUrl is the SSE endpoint for one task's live agent events (AT2),
// consumed by useTaskEvents via the browser EventSource API (cookie auth, no
// custom headers — Last-Event-ID replay is automatic on reconnect).
export function taskEventsUrl(machineID: string, taskID: string): string {
  return `/api/machines/${encodeURIComponent(machineID)}/tasks/${encodeURIComponent(taskID)}/events`;
}

// SSE endpoint for live machine state; consumed by useMachineEvents via the
// browser EventSource API (cookie auth, no custom headers).
export const machineEventsUrl = '/api/machine/events';

// The login redirect is a full navigation (not fetch) so the browser follows
// the IdP's 302 chain and cookies are set on the top-level document. Login is
// Zitadel OIDC (TAV-149); Connect GitHub is the post-login linking flow that
// unlocks git operations.
export const loginUrl = '/api/auth/login';
export const githubConnectUrl = '/api/auth/github/connect';
