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

// Placeholder for the Phase 2 machine summary; null for the whole of Phase 1.
export type MachineSummary = Record<string, never>;

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
};

// The login redirect is a full navigation (not fetch) so the browser follows
// GitHub's 302 chain and cookies are set on the top-level document.
export const loginUrl = "/api/auth/github/login";
