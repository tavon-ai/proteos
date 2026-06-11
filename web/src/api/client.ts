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
  type: "transition" | "error" | "info";
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
};

// SSE endpoint for live machine state; consumed by useMachineEvents via the
// browser EventSource API (cookie auth, no custom headers).
export const machineEventsUrl = "/api/machine/events";

// The login redirect is a full navigation (not fetch) so the browser follows
// GitHub's 302 chain and cookies are set on the top-level document.
export const loginUrl = "/api/auth/github/login";
