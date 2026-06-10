# Phase 1 Implementation Plan: Control-plane skeleton + GitHub auth

> Derived from `plans/proteos-poc-to-prod.md` (Phase 1). Goal: a user signs in with
> GitHub and lands on a dashboard showing "no machine yet". Establishes the repo
> structure, Go control plane, React SPA, Postgres migrations, sessions, and CI that
> every later phase builds on. No compute.

## Decisions made in this phase

| Decision | Choice | Rationale |
|---|---|---|
| GitHub App vs classic OAuth app | **GitHub App** (with "Request user authorization (OAuth) during installation" enabled; web login uses the App's user-authorization flow) | Master plan calls for deciding now: per-repo installation grants and short-lived refreshable user tokens are exactly what the Phase 7 credential broker needs; re-onboarding users off a classic OAuth app later is painful. Login UX is identical. |
| Repo layout | **Monorepo, this repo.** New code in `controlplane/` (Go) and `web/` (React). PoC (`server/`, `public/`, dockerfiles) stays untouched as the behavior reference until Phase 9 retires it. | One repo keeps the React↔Go contract, migrations, and CI in lockstep; the master plan adds `node-agent/` and `guest-agent/` beside them in Phase 2/3. |
| Secrets in Phase 1 | **Stubbed secrets interface** (`secrets.Store`), file-backed dev implementation. OpenBao implements the same interface in Phase 5. | Explicitly sanctioned by the Phase 1 acceptance criteria. The hard requirement honored now: GitHub tokens never land in Postgres — `github_links` stores only a `secret_ref`. |
| Go HTTP stack | stdlib `net/http` (1.22+ pattern routing) + small middleware; `pgx` + `sqlc`; `golang-migrate` for migrations | Master plan tech baseline; no framework needed for ~10 routes, and the gateway (Phase 3) wants raw `net/http` control anyway. |
| Frontend stack | Vite + React + TypeScript, React Router, TanStack Query | Master plan tech baseline ("a router, a data-fetching layer"). |
| Sessions | Opaque 256-bit token, **SHA-256 hash stored** in `sessions`; cookie `httpOnly; Secure; SameSite=Lax; Path=/` | Server-side sessions per master plan; hashing means a DB leak doesn't leak live sessions. |
| CSRF | `SameSite=Lax` + require custom header (`X-Requested-By`) on state-changing routes; OAuth `state` in a signed, short-lived cookie validated on callback | Lax blocks cross-site POSTs; the header check covers the gap; no token plumbing needed in the SPA. |
| CI | GitHub Actions: Go build/vet/test (with Postgres service container, migrations applied), `sqlc diff`, web typecheck + build | Master plan: CI starts in Phase 1, every PR. |

## Target layout

```
controlplane/
  go.mod
  cmd/controlplane/main.go        # flag/env config, wire deps, serve
  internal/
    config/                       # env parsing, validation
    httpapi/                      # router, handlers, middleware (auth, CSRF, logging)
    auth/                         # GitHub OAuth flow, state handling
    session/                      # create/lookup/revoke sessions
    store/                        # sqlc-generated queries + pgx pool
    secrets/                      # Store interface + filestore (dev stub)
    github/                       # minimal GitHub API client (user profile, token exchange)
  migrations/                     # golang-migrate SQL files
  sqlc.yaml
web/
  package.json, vite.config.ts    # dev server proxies /api → :8080
  src/
    api/                          # typed fetch client + TanStack Query hooks
    routes/                       # /login, /  (dashboard)
    components/
compose.dev.yml                   # Postgres 16 for local dev
.github/workflows/ci.yml
docs/github-app-setup.md          # manual: create the GitHub App, callback URLs, env vars
```

## Tasks

### 1.0 Scaffolding + CI skeleton
- `controlplane/` Go module; `cmd/controlplane` serving `/healthz`.
- `web/` Vite React-TS app; dev proxy `/api` → `localhost:8080`.
- `compose.dev.yml` with Postgres 16 (named volume, port 5432).
- `.github/workflows/ci.yml`: Go build + vet + test, web `tsc --noEmit` + `vite build`.
  CI must be green on the first PR; every later task lands through it.

**Done when:** fresh clone → `docker compose -f compose.dev.yml up -d` →
`go run ./cmd/controlplane` + `npm run dev` serves a placeholder page; CI passes.

### 1.1 Postgres: migrations + store
- Migrations (via `golang-migrate`, run by the binary on startup with `--migrate` and by CI):
  - `users` — id (uuid), github_user_id (bigint, unique), login, email, avatar_url,
    status, created_at.
  - `sessions` — id (uuid), user_id FK, token_hash (bytea, unique), created_at,
    expires_at, revoked_at.
  - `github_links` — user_id FK (pk), scopes/installation metadata (jsonb),
    secret_ref (text — path into the secrets store), created_at, updated_at.
- `sqlc` config + queries: upsert user by `github_user_id`, create/lookup-by-hash/revoke
  session, upsert `github_links`.
- Store tests run against **Testcontainers** Postgres (no mocks, per master plan); CI uses
  a service container with the same migrations.

**Done when:** migrations apply cleanly up and down; store tests pass locally and in CI.

### 1.2 Sessions + auth middleware
- `session` package: `Create(userID) (token, error)` (random 32 bytes, store SHA-256),
  `Authenticate(token)` (hash lookup, expiry/revocation check), `Revoke`.
- Cookie handling: `proteos_session`, httpOnly, Secure, SameSite=Lax, 30-day expiry,
  sliding refresh on use. (Secure works on `localhost` in modern browsers — no dev flag.)
- Middleware: `RequireAuth` (401 JSON on missing/invalid session) and `CSRFHeader`
  (reject state-changing methods without `X-Requested-By: proteos`).
- Route guard wired so **all** `/api/machine*` paths 401 when unauthenticated —
  register the guard on the path prefix now, even though the handlers are stubs,
  so Phase 2 inherits it instead of adding it.

### 1.3 GitHub App + OAuth flow
- Manual setup doc `docs/github-app-setup.md`: create the GitHub App (user authorization
  enabled, refresh tokens enabled, callback `https://<host>/api/auth/github/callback` and
  the localhost equivalent), record `GITHUB_APP_CLIENT_ID` / `GITHUB_APP_CLIENT_SECRET`.
- `GET /api/auth/github/login`: generate `state` (random, HMAC-signed, 10-min expiry) in
  a short-lived cookie; redirect to GitHub authorize URL.
- `GET /api/auth/github/callback`: validate `state` against the cookie (constant-time),
  exchange code for user access + refresh tokens, fetch the GitHub user, upsert `users`,
  write tokens to the secrets store, upsert `github_links.secret_ref`, create session,
  set cookie, redirect to `/`.
- `POST /api/auth/logout`: revoke session, clear cookie.
- Tests drive the full flow against an `httptest` fake of GitHub's authorize/token/user
  endpoints (no mocks of *our* code; fake only the external boundary). Cover: happy path,
  bad/expired `state`, GitHub error response, repeat login (upsert, fresh session).

### 1.4 Secrets store (stub)
- `secrets.Store` interface: `Put(path string, data map[string]string)`, `Get`, `Delete` —
  paths follow the master plan convention (`secret/users/<user_id>/github`) so the
  OpenBao implementation in Phase 5 is a drop-in.
- Dev implementation: single JSON file under a gitignored `.data/` dir, 0600, loud
  startup log line `"secrets: using DEV file store — not for production"`.
- Enforced invariant (asserted in a test): no token-shaped value is ever written through
  the `store` package; `github_links` holds only `secret_ref`.

### 1.5 API surface
- `GET /api/me` → `{ user: {login, email, avatar_url}, machine: null }` (machine summary
  hardcoded `null` this phase; shape matches the master plan so Phase 2 fills it in).
- `GET /api/machine` → `404 {"error":"no_machine"}`; `POST /api/machine`,
  `/start`, `/stop` → `501 {"error":"not_implemented"}` — all behind `RequireAuth`.
- Consistent JSON error envelope `{"error": string}` from day one; request logging
  middleware (no tokens/cookies in logs).

### 1.6 React SPA
- Routes: `/login` (unauthenticated: "Sign in with GitHub" → `/api/auth/github/login`)
  and `/` (dashboard).
- Boot: query `/api/me`; 401 → redirect to `/login`; authed → dashboard with user
  identity and the **"no machine yet" empty state** (placeholder "Create machine" button,
  disabled/explained — Phase 2 enables it).
- Logout button → `POST /api/auth/logout` (with the CSRF header) → back to `/login`.
- Typed API client in `src/api/` that always sends `X-Requested-By` and treats 401 as a
  session-expired signal globally.

### 1.7 CI completion + acceptance pass
- Extend CI: migrations applied against the service Postgres before tests; `sqlc` drift
  check; Go test with `-race`; web build artifact uploaded.
- Walk every Phase 1 acceptance criterion from the master plan end-to-end and check it
  off in `plans/proteos-poc-to-prod.md`.

## Sequencing & parallelism

```
1.0 ──► 1.1 ──► 1.2 ──► 1.3 ──► 1.5 ──► 1.7
         │               ▲
         └─ 1.4 ─────────┘        1.6 (start after 1.0; integrate after 1.5)
```

1.4 (secrets stub) is independent after 1.1 and must exist before 1.3's callback writes
tokens. 1.6 can develop against a hand-stubbed `/api/me` immediately after 1.0.

## Test strategy

- **Store layer**: Testcontainers Postgres, real migrations, no mocks.
- **Auth flow**: end-to-end `httptest` server with a fake GitHub; assert cookies, state
  validation, DB rows, secrets-store contents.
- **Authorization**: table-driven test that every `/api/machine*` route and `/api/me`
  returns 401 without a session and 403/skip without the CSRF header on mutations.
- **Frontend**: typecheck + build in CI is the bar for this phase; component tests are
  not required yet (the dashboard is an empty state).

## Risks / notes

- **GitHub App callback URLs**: one App supports multiple callback URLs — register both
  localhost and the deployed host in the same App to avoid maintaining two Apps in dev.
- **Refresh-token handling is stored but not yet exercised** — token refresh logic is
  Phase 7 (credential broker). Phase 1 only stores access+refresh tokens at login.
- **Any public deployment of this phase stays behind the signup allowlist** rule from the
  master plan's cross-cutting notes — simplest form: an `ALLOWED_GITHUB_LOGINS` env list
  checked at callback time, returning a "not invited" page. Cheap now, required before
  anything is internet-facing.
- The PoC server still runs on its own (`npm start`) and is untouched; nothing in Phase 1
  imports from it.
