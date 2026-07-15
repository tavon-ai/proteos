# Gitea/Forgejo Phase 2 Implementation Plan: PATs, private repos, push, PRs

> Source: phase 1 follow-up, planned 2026-07-13. Decisions taken with the user:
> **PAT per host** (no OAuth2 app registration on Gitea/Forgejo instances) and
> **push + create-PR scope** (the PR review window — files/checks/merge — stays
> GitHub-only and becomes phase 3).
> Status: **all workstreams implemented**, 2026-07-13: git_host_links
> migration + queries, secret path, gitea client, PAT API, per-host credential
> broker, PR dispatch, web Settings → Git hosting panel, CLI `git hosts`
> commands, guest/deploy copy sweep, and the guest e2e PAT-push acceptance.
> Phase 3 seams (PR review surface, repo listing, OAuth2) are listed at the
> end of this document.
> Prerequisite: phase 1 landed (PRs #52, #53) — clone-by-URL from
> `PROTEOS_GIT_PUBLIC_HOSTS`, per-repo host via `parseRemote`, and the
> `unsupported_host` guard in `handleGitPR`.

## Context

Phase 1 made allowlisted hosts reachable anonymously. Phase 2 adds a per-user,
per-host **Personal Access Token** so those hosts get the full write loop:
private clones, pushes, and PR creation via Gitea's `/api/v1` (Forgejo is
API-identical for everything this phase touches). GitHub behavior is unchanged
throughout, and a user who never adds a PAT keeps exactly the phase 1
experience.

Facts that anchor the design:

1. **The phase 1 seams are in place.** `parseRemote` (`httpapi/gitops.go`)
   returns the remote's host, and `handleGitPR`'s `unsupported_host` check is
   the declared provider-dispatch point. The credential broker
   (`guestctl/manager.go` `handleCredential`) refuses every non-auth host
   today — that refusal becomes a per-host lookup.
2. **PATs are radically simpler than the GitHub token machinery.** GitHub's
   `TokenSource` exists because App tokens expire in ~8h with rotating refresh
   tokens (`github/tokensource.go`). Gitea PATs don't expire and don't rotate:
   no refresh flow, no per-user singleflight lock, no proactive expiry skew. A
   plain store-backed lookup suffices; a revoked PAT surfaces as a 401 at use
   time.
3. **Both storage conventions already exist.** Links: `github_links(user_id,
   metadata, secret_ref)` + a secret blob (`secrets.UserGitHubPath`). API
   shape: `PUT/DELETE /api/secrets/providers/{key}` (`server.go:191-192`)
   validates and stores per-user provider keys. Phase 2 composes the two.
   NOTE: migration `000013_user_git_identity` is the *commit* identity
   (name/email) — the new table is named `git_host_links` to avoid collision.
4. **Gitea auth is two-sided.** API calls use `Authorization: token <PAT>`.
   git-over-https uses HTTP Basic with the user's **login** as username and
   the PAT as password — so the login must be captured at PAT-save time
   (`GET /api/v1/user`) and stored alongside the token. That validation call
   doubles as the PAT check.
5. **No new deployment config.** PATs are per-user data through the existing
   secrets store — nothing to add to compose/.env ([[proteos-new-env-vars-need-
   compose-plumbing]] does not bite here). The only deploy-adjacent change is
   copy: `PROTEOS_GIT_PUBLIC_HOSTS` comments currently say "anonymous clone
   only", which stops being the whole truth.

Design decision — **keep `PROTEOS_GIT_PUBLIC_HOSTS` as the single allowlist**.
A PAT may only be registered for a host already on it, so the operator retains
control of which hosts guests can talk to; the env var name stays (renames
break deployments), with comments updated to "additional git hosts; anonymous
public reads by default, per-user PATs unlock private repos, push, and PRs."

## Workstreams

### 1. Data model: `git_host_links`

`controlplane/migrations/000015_git_host_links.{up,down}.sql`

- `git_host_links (user_id uuid REFERENCES users, host text, metadata jsonb
  NOT NULL DEFAULT '{}', secret_ref text NOT NULL, created_at, updated_at,
  PRIMARY KEY (user_id, host))`. `host` is the lowercased `host[:port]` form
  the allowlist and `parseRemote` both produce; `metadata` holds the
  non-sensitive login (for display) — the token never leaves the secret store.
- `store/queries.sql`: `UpsertGitHostLink`, `GetGitHostLink(user, host)`,
  `ListGitHostLinks(user)`, `DeleteGitHostLink(user, host)` + sqlc regen.

### 2. Secrets path

`controlplane/internal/secrets/secrets.go`

- `UserGitHostPath(userID, host)` → `secret/users/<id>/githosts/<host>` with
  `:` sanitized to `_` (ports in OpenBao path segments). Fields: `{token,
  login}`. Lives under the user subtree, so the existing user-scoped OpenBao
  policy covers it with no policy change (same argument as `UserProfilePath`).

### 3. Gitea API client

`controlplane/internal/gitea/` (new package; one client serves Forgejo too)

- `Client{BaseURL string}` constructed per host (`https://<host>/api/v1`),
  auth header `Authorization: token <PAT>` per call.
- `GetUser(ctx, token) (login string, err)` — PAT validation + login capture.
- `GetRepo(ctx, token, owner, repo)` — default-branch resolution for PRs
  (works tokenless for public repos; always send the token when present).
- `CreatePR(ctx, token, owner, repo, head, base, title, body)` — `POST
  /repos/{owner}/{repo}/pulls`. Error mapping: 401 → `ErrBadToken`; 409 →
  `ErrPRAlreadyExists`; the "no diff" case → `ErrNoPRCommits` (Gitea signals
  it via 409/422 with a message — pin the exact behavior in the client test
  against a fake, and tolerate both statuses).
- Mirror the `github.Client` testing seam: injectable base URL, httptest fake.

### 4. PAT management API

`controlplane/internal/httpapi/githosts.go` (new), routes beside the provider
key endpoints, all `requireAuth` + CSRF:

- `GET /api/git/hosts` → `{hosts: [{host, linked, login}]}` — one row per
  allowlisted entry in `s.GitPublicHosts`, joined against `ListGitHostLinks`.
- `PUT /api/git/hosts/{host}/token` `{token}` → host must be on the allowlist
  (404 `unknown_host` otherwise); validate via `gitea.GetUser`; store
  `{token, login}` at `UserGitHostPath`; upsert `git_host_links` with
  `{login}` metadata. 400 `bad_token` on a 401 from the host; 502
  `githost_unavailable` if unreachable. Audit `git.host.token.set`.
- `DELETE /api/git/hosts/{host}/token` → delete secret + link row. Audit.
- New audit action constants in `internal/audit`.

### 5. Credential broker: per-host resolution

`controlplane/internal/guestctl/manager.go`

- New narrow interface (defined in guestctl, satisfied by a small store+secrets
  adapter — do NOT couple guestctl to the gitea package):
  `HostCredentialSource{ HostCredential(ctx, userID, host) (username, password
  string, err) }`, with `ErrNoHostLink` for "no PAT".
- `handleCredential` dispatch: `req.Host == m.gitHost` → existing GitHub path,
  byte-for-byte. Else if `req.Host` ∈ configured public hosts: resolve the
  link; found → `{Username: login, Password: PAT}` (no Expiry — PATs don't
  expire); not found → `forbidden_host` (same code as today — the helper's
  message already says public hosts are limited; sharpen it to mention adding
  a token, WS8). Host not allowlisted → `forbidden_host` unchanged.
- `Manager.New` gains the resolver and the public-hosts list (nil/empty ⇒
  phase 1 behavior). `main.go` wires both.
- Audit `git.credential` entries already record the host — no change.

### 6. PR dispatch by host

`controlplane/internal/httpapi/gitops.go` `handleGitPR`

- Replace the `unsupported_host` guard with three-way dispatch:
  - host == `s.GitHost` → existing GitHub path, untouched.
  - host ∈ `s.GitPublicHosts` → resolve the user's PAT (no link → 409
    `githost_token_required`); `gitea.GetRepo` for the default base;
    `gitea.CreatePR`. Map: `ErrBadToken` → 409 `githost_token_invalid`,
    `ErrPRAlreadyExists` → 409 `pr_exists`, `ErrNoPRCommits` → 422
    `no_commits`, transport → 502 `githost_unavailable`.
  - anything else → 422 `unsupported_host` (unchanged).
- The server needs a per-host gitea client factory (`func(host) *gitea.Client`)
  rather than a single client — a small field on `Server`, defaulted in
  `main.go`, overridable in tests.

### 7. Web: git-hosts settings + copy

- `client.ts`: `listGitHosts`, `setGitHostToken(host, token)`,
  `deleteGitHostToken(host)`; error codes `bad_token`, `unknown_host`,
  `githost_token_required`, `githost_token_invalid`.
- Settings window: a "Git hosts" section listing each allowlisted host with
  linked login or a token input (mirror the provider-keys panel UX — masked
  input, save validates, clear button). Empty allowlist ⇒ section hidden.
- Launcher copy: the public-host clone-failure hint ("only public repos…")
  becomes conditional — if the host is linked, say "check the repo path or
  your token's scopes"; if not, link to Settings → Git hosts.
- PR window: no structural change — once WS6 lands, `git pr` just works for
  gitea projects. Map the two new 409 codes to actionable messages.

### 8. CLI + guest copy

- `proteos git hosts ls` (host, linked, login), `proteos git hosts set-token
  <host>` (token via `--token-stdin` or prompt — never argv, it leaks into
  shell history/ps), `proteos git hosts rm-token <host>`. New
  `cli/internal/client/api_githosts.go` + `app/githost.go`, wired under the
  existing `git` group.
- Guest credential-helper `forbidden_host` message (`gitcredential.go`):
  append "…add a token for this host in ProteOS Settings to push or access
  private repos."
- Comment sweep: `config.go` `GitPublicHosts` doc, `compose.yaml` /
  `.env.example` comments (drop "anonymous clone only"; NO new env var).

### 9. Tests

- **gitea client**: httptest fake covering GetUser (200/401), GetRepo,
  CreatePR (created / exists / empty-diff / 401).
- **PAT endpoints**: handler tests — save validates against the fake and
  persists login; bad token 400; unknown host 404; delete removes both rows;
  GET reflects linked state.
- **guestctl**: extend `manager_test.go` — credential for a linked public
  host returns login+PAT; unlinked public host and unknown host both refuse
  with `forbidden_host`; auth-host path unchanged.
- **PR dispatch**: `gitops_test.go` — gitea project + linked PAT creates the
  PR against a fake `/api/v1` (asserting the GitHub fake is never hit);
  unlinked → `githost_token_required`; 401 from host → `githost_token_invalid`.
- **Guest e2e**: variant of `TestGitCredentialHelper_CloneCommitPush` where
  only receive-pack is auth-gated (the phase 1 `publicReadGate`) and the
  resolver returns login+PAT — anonymous clone, authenticated push lands.
- **CLI/web**: fake-CP tests for the new commands; vitest for the settings
  panel state mapping.
- **Regression**: full suites; a user with no PATs and a deployment with an
  empty allowlist must be behaviorally identical to phase 1.

## Sequencing

1. WS1+2 (migration, queries, secret path) — pure plumbing, no behavior.
2. WS3 (gitea client) + WS4 (PAT API) — user-visible: tokens can be saved and
   validated; demoable via curl.
3. WS5 (credential broker) — private clone + push start working end-to-end.
4. WS6 (PR dispatch) — the write loop completes.
5. WS7+8 (web, CLI, copy) — surfaces.
6. WS9 e2e + regression sweep.

Slices 1–2 make a natural first PR; 3–4 a second; 5–6 a third — each leaves
main shippable.

## Acceptance criteria

- `PUT /api/git/hosts/gitea.alacasa.uk/token` with a valid PAT stores it and
  `GET /api/git/hosts` shows `linked: true` with the Gitea login; a bad PAT is
  rejected 400 without storing anything.
- A **private** repo on a linked host clones by URL; the same repo without a
  PAT fails at the git layer with the sharpened helper message.
- `git push` from a guest to a linked host succeeds (e2e-proven); to an
  unlinked public host it still fails with the anonymous-clone-only message.
- "Open PR" on a gitea project returns the Gitea PR URL; without a PAT it
  returns 409 `githost_token_required`; GitHub PRs are untouched.
- Empty allowlist / no-PAT deployments are behaviorally identical to phase 1;
  all suites green.

## Phase 3 seams left behind

- The PR review window for Gitea/Forgejo: `GET /repos/{o}/{r}/pulls/{n}`,
  `…/files`, commit-status (Gitea's check-runs equivalent), and merge — the
  `Server` gitea-client factory and per-host dispatch in WS6 are where it
  attaches.
- Repo listing (`GET /api/v1/user/repos`) for a picker beside add-by-URL.
- OAuth2 apps per host, if PAT friction ever warrants it.
