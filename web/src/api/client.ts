// Typed fetch client for the control-plane API.
//
// Every request carries the X-Requested-By header (the CSRF defense that pairs
// with SameSite=Lax cookies), and a 401 is surfaced as a typed
// SessionExpiredError so the app can redirect to /login globally.

export class SessionExpiredError extends Error {
  constructor() {
    super("session expired");
    this.name = "SessionExpiredError";
  }
}

export class ApiError extends Error {
  status: number;
  code: string;
  constructor(status: number, code: string) {
    super(`api error ${status}: ${code}`);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
  }
}

export interface Me {
  user: {
    login: string;
    email: string;
    avatar_url: string;
  };
  machine: MachineSummary | null;
}

// MachineState mirrors the control-plane machines.state CHECK constraint.
export type MachineState =
  | "requested"
  | "provisioning"
  | "running"
  | "starting"
  | "stopping"
  | "hibernating"
  | "stopped"
  | "error";

// SnapshotSummary is the current hibernation snapshot metadata (Phase 4),
// present only while the machine is hibernated (stopped with a usable snapshot).
export interface SnapshotSummary {
  fc_version: string;
  mem_bytes: number;
  created_at: string;
}

export interface MachineSummary {
  id: string;
  state: MachineState;
  guest_ip: string | null;
  kernel_ref: string;
  rootfs_ref: string;
  resource_spec: { vcpus: number; mem_mib: number; disk_mib?: number };
  last_error: string | null;
  created_at: string;

  // Phase 4: persistent disk + hibernate/resume.
  boot: "cold" | "resumed" | null;
  disk_id: string | null;
  disk_mib: number | null;
  snapshot: SnapshotSummary | null;
}

export interface MachineEvent {
  id: number;
  // "git.clone" is a Phase 7 info-style event carrying a clone completion
  // (payload: { op_id, ok, detail }).
  type: "transition" | "error" | "info" | "git.clone";
  from_state: string | null;
  to_state: string | null;
  actor: string;
  payload: Record<string, unknown>;
  created_at: string;
}

// SSE payloads from GET /api/machine/events.
export interface SnapshotData {
  machine: MachineSummary | null;
  events: MachineEvent[];
}
export interface MachineEventData {
  machine: MachineSummary;
  event: MachineEvent;
}

// SecretField is one declared input a provider needs (Phase 6). The settings UI
// renders a form from these — name is the field key, label is the prompt, env is
// the variable it becomes inside the machine. None of these are secret.
export interface SecretField {
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

// DesktopLayout is the opaque serialized window layout stored in machine SQLite
// (Phase 9 decision #6). The control plane relays it verbatim; only the desktop
// understands its shape. null ⇒ no layout saved yet.
export interface DesktopResponse {
  layout: unknown | null;
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: {
      "X-Requested-By": "proteos",
      ...(init.headers ?? {}),
    },
    credentials: "same-origin",
  });

  if (res.status === 401) {
    throw new SessionExpiredError();
  }
  if (!res.ok) {
    let code = "error";
    try {
      const body = (await res.json()) as { error?: string };
      if (body.error) code = body.error;
    } catch {
      // non-JSON error body; keep the generic code
    }
    throw new ApiError(res.status, code);
  }
  // 204 / empty body tolerance.
  const text = await res.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

export const api = {
  me: () => request<Me>("/api/me"),
  logout: () => request<void>("/api/auth/logout", { method: "POST" }),

  // GET /api/machine returns the user's machine, or null when they have none
  // (the API answers 404 no_machine, which we translate to null here).
  getMachine: async (): Promise<MachineSummary | null> => {
    try {
      return await request<MachineSummary>("/api/machine");
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) return null;
      throw err;
    }
  },
  createMachine: () => request<MachineSummary>("/api/machine", { method: "POST" }),
  startMachine: () => request<MachineSummary>("/api/machine/start", { method: "POST" }),
  stopMachine: () => request<MachineSummary>("/api/machine/stop", { method: "POST" }),

  // Mint a one-shot editor URL for the running machine (Phase 8). 409
  // machine_not_running / 404 no_machine surface as ApiError. Only available when
  // the control plane has PROTEOS_MACHINE_DOMAIN set; otherwise the route 404s.
  // An optional `folder` opens code-server directly on a project (Phase 9 #5);
  // 400 bad_folder if it is not a listable project.
  webSession: (folder?: string) =>
    request<WebSession>("/api/machine/web-session", {
      method: "POST",
      headers: folder ? { "Content-Type": "application/json" } : {},
      body: folder ? JSON.stringify({ folder }) : undefined,
    }),

  // Projects + desktop layout (Phase 9). listProjects 409s when the machine is
  // not running. getDesktop/putDesktop relay the opaque layout to/from machine
  // SQLite; putDesktop is a 204 (a no-op on a diskless stack).
  listProjects: () => request<ProjectsResponse>("/api/projects"),
  getDesktop: () => request<DesktopResponse>("/api/machine/desktop"),
  putDesktop: (layout: unknown) =>
    request<void>("/api/machine/desktop", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ layout }),
    }),

  // Providers + write-only secret keys. setProviderKey/deleteProviderKey return
  // 204; values are never echoed back. fields maps each declared secret field
  // name to its value (Phase 6 generalizes Phase 5's single api_key body).
  listProviders: () => request<Provider[]>("/api/providers"),
  setProviderKey: (key: string, fields: Record<string, string>) =>
    request<void>(`/api/secrets/providers/${encodeURIComponent(key)}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ fields }),
    }),
  deleteProviderKey: (key: string) =>
    request<void>(`/api/secrets/providers/${encodeURIComponent(key)}`, { method: "DELETE" }),

  // Git operations (Phase 7). listRepos may throw ApiError 409 reconnect_github
  // when the GitHub grant is revoked; cloneRepo returns an op_id and the clone
  // completes asynchronously (watch git.clone machine events).
  listRepos: () => request<ReposResponse>("/api/git/repos"),
  cloneRepo: (fullName: string) =>
    request<CloneStarted>("/api/git/clone", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ full_name: fullName }),
    }),
};

// SSE endpoint for live machine state; consumed by useMachineEvents via the
// browser EventSource API (cookie auth, no custom headers).
export const machineEventsUrl = "/api/machine/events";

// The login redirect is a full navigation (not fetch) so the browser follows
// GitHub's 302 chain and cookies are set on the top-level document.
export const loginUrl = "/api/auth/github/login";
