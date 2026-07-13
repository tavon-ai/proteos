# Gitea/Forgejo Phase 1 Implementation Plan: public repos, per-repo host

> Source: multi-provider git hosting discussion, planned 2026-07-13.
> Status: **all workstreams implemented** — uncommitted, 2026-07-13. Config
> allowlist, clone-by-URL, PR-path host guard, guest copy sweep, web launcher
> URL refs, CLI URL refs, and the guest e2e (anonymous public-host clone +
> push refused with the anonymous-clone-only message). The CP-side live-stack
> walk (clone via API → SSE event on a second host) remains a manual/KVM-runner
> acceptance; the in-repo pieces covering it are the clone handler tests, the
> guestctl foreign-host credential-refusal test, and the guest e2e.
> Scope: clone **public** repos from operator-allowlisted Gitea/Forgejo hosts
> (they share one API and one behavior for everything this phase touches), with
> the host threaded **per repo** rather than per deployment. Explicitly out of
> scope (Phase 2): Gitea/Forgejo OAuth/tokens, private repos, push, and PR
> creation on non-GitHub hosts. GitHub behavior is unchanged throughout.

## Context

ProteOS today assumes exactly one git host per deployment. `PROTEOS_GIT_HOST`
(default `github.com`) is a single config value (`config.go:48-51`); clone URLs
are built from it (`httpapi/git.go:136`); and the guest credential broker
refuses any other host (`guestctl/manager.go:326`). Repo discovery is
GitHub-App-specific (`github/client.go` `ListUserRepos` walks
`/user/installations`), so there is no way to reach a repo on any other host.

Public repos let us break the single-host assumption cheaply, because the
expensive parts of multi-provider support — OAuth, token storage, provider API
clients — are not needed to *read* a public repo:

1. **All mutating git ops already run the real `git` CLI in the guest** over
   plain HTTPS (`guestwire.go` ops `git.clone` etc.). A public Gitea/Forgejo
   repo clones anonymously; git never issues a credential-helper callback
   without a 401 challenge, so the credential broker is not even invoked.
2. **The clone dispatch is already URL-shaped.** `GitChannel.Clone(ctx,
   machineID, url, dest, opID)` (`httpapi/git.go:23-26`) carries a full URL;
   the guest is host-agnostic today. Only the CP-side URL *construction* is
   host-pinned.
3. **A project's host is recoverable from its origin remote.** The worktree
   surface already resolves `origin` per project
   (`gitops.go:resolveProjectRemote`), so per-repo host needs **no DB schema
   change** — the remote URL is the source of truth. The `github_links` table
   stays untouched until Phase 2 adds non-GitHub identities.
4. **One latent bug becomes live the moment a second host exists:**
   `parseOwnerRepo` (`gitops.go:419-441`) deliberately discards the host, so
   `handleGitPR` would take a project cloned from `codeberg.org/foo/bar` and
   call **GitHub's** API for `foo/bar` — the wrong repo, possibly someone
   else's. Per-repo host threading must land in the PR path *as a guard* in
   this phase, even though non-GitHub PR creation itself is Phase 2.

Design decision — config shape: keep `PROTEOS_GIT_HOST` exactly as-is (the
*authenticated* host: credentials minted, repos listed, PRs created), and add
`PROTEOS_GIT_PUBLIC_HOSTS` — a comma-separated allowlist of additional hosts
(e.g. `codeberg.org,git.example.com:3000`) that support **anonymous clone
only**. An empty list preserves today's behavior bit-for-bit. This keeps the
security property documented at `git.go:98` ("the URL is host-pinned") — clones
can only ever target operator-configured hosts, never an arbitrary
user-supplied one, which also keeps the guest from being pointed at internal
network hosts (SSRF-shaped abuse).

## Workstreams

### 1. Config: the public-hosts allowlist

`controlplane/internal/config/config.go`

- Add `GitPublicHosts []string` next to `GitHost` (`config.go:48-51`), parsed
  from `PROTEOS_GIT_PUBLIC_HOSTS` (comma-separated, trimmed, empties dropped).
- Validate each entry as `host` or `host:port` (hostname chars + optional
  numeric port — self-hosted Gitea commonly runs on a non-443 port). Reject
  schemes, paths, and credentials in the value; fail startup loudly on a bad
  entry rather than silently narrowing the allowlist.
- Thread into `httpapi.Server` as `GitPublicHosts` beside the existing
  `GitHost` field (`server.go:79-87`).
- Document in the config comment: anonymous clone only; no credentials are
  ever minted for these hosts in this phase.

### 2. Clone by URL: `POST /api/git/clone`

`controlplane/internal/httpapi/git.go`

- Extend `cloneRequest` with `URL string` (`json:"url"`), mutually exclusive
  with `FullName`. Exactly one must be set; both or neither ⇒ `bad_request`.
  - `full_name` keeps today's exact semantics: GitHub-picker path, URL built
    from `s.GitHost` (`git.go:136`). Zero behavior change.
  - `url` is the new path: accept `https://host[:port]/owner/repo[.git]`
    (and tolerate a trailing slash). Parse with `net/url`, then validate:
    scheme must be `https`, host must be in `GitPublicHosts` **or equal
    `s.GitHost`** (so a full GitHub URL pasted into the new box still works),
    path must reduce to exactly `owner/repo` matching the existing
    `fullNameRe` (`git.go:30`) — this inherits the traversal rejection that
    protects the `/workspace/` dest. Reject userinfo (`user@host`) outright.
  - **Rebuild the clone URL from the validated parts** (`https://` + host +
    `/owner/repo.git`), never forward the raw user string — that is what
    preserves the host-pinning property.
  - Error codes: `forbidden_host` (host not allowlisted), `bad_url`
    (unparseable/wrong shape). Keep `bad_full_name` for the existing path.
- Factor the parse/validate into `parseCloneURL(raw string, gitHost string,
  publicHosts []string) (host, fullName string, err)` so the CLI/web can get
  identical errors and it unit-tests without a server.
- Dest stays `"/workspace/" + repoDir(fullName)` — same-name collisions across
  hosts behave exactly like today's same-name collisions across owners
  (documented, not solved here).
- Audit: add `"host"` to the `ActionGitClone` metadata (`git.go:148-154`) so
  the log distinguishes providers.
- Update the `handleGitClone` doc comment (`git.go:95-101`): the URL is now
  pinned to *the allowlist* rather than the single host; anonymous public
  clone from extra hosts is the intended worst case.

### 3. PR path: per-repo host guard (the bug fix)

`controlplane/internal/httpapi/gitops.go`

- Generalize `parseOwnerRepo` → `parseRemote(remote) (host, owner, repo string,
  ok bool)`: same https + scp-like coverage, but *keep* the host instead of
  discarding it. Existing callers that only need owner/repo ignore the host.
  Extend `parseownerrepo_internal_test.go` with host-assertion cases
  (https with port, scp-like, `.git` suffix).
- In `handleGitPR` (`gitops.go:309`): after resolving the project remote,
  compare the parsed host against `s.GitHost`. Mismatch ⇒ HTTP 422
  `unsupported_host` — *before* any token resolution or GitHub API call. This
  is the guard for anchor fact #4; when Phase 2 lands a Gitea provider client,
  this comparison becomes the provider-dispatch point.
- Same guard in any other CP→GitHub call site keyed by a project remote (audit
  `gitops.go` for siblings of `handleGitPR`; the PR-review surface in
  `github/pulls.go` consumers is GitHub-only by construction but must fail
  with `unsupported_host`, not misroute).

### 4. Guest side: comments and error copy only

No functional guest change: public clones never trigger the credential helper,
and `handleCredential` (`guestctl/manager.go:321-355`) correctly keeps minting
tokens **only** for `m.gitHost`. A private/push attempt against a public host
gets a 401 → helper → `forbidden_host` → failed op event, which is the correct
Phase 1 outcome (read-only), but the copy must not gaslight the user:

- `guestagent/cmd/guestagent/gitcredential.go:106`: reword "only provides
  credentials for github.com over https" to say credentials are only available
  for the configured auth host, and that pushes/private fetches on public
  hosts need Phase 2.
- `guestagent/api/guestwire.go:603` (`Host` field comment) and the
  `ErrCodeForbiddenHost` comment (`guestwire.go:529-531`): update to reflect
  "the configured auth host" vs "an allowlisted public host".
- `controlplane/internal/guestctl/manager.go:63-64` (`New` doc): `gitHost` is
  now "the only host credentials are minted for" — no longer "the only host
  clones target".
- Optional polish: append common public hosts to the SSH known-hosts
  convenience list in `profile/profile.go:144` is **not** needed (clones are
  https-only), skip it.

### 5. Web UI: clone-from-URL

`web/src/desktop/ProjectsLauncher.tsx`, `web/src/api/client.ts`,
`web/src/desktop/repoRef.ts`

- `client.ts`: extend the clone call to send `{url}` when given a URL-shaped
  ref, `{full_name}` otherwise.
- `ProjectsLauncher.tsx`: add a "Clone from URL" input beside the GitHub repo
  picker (paste `https://codeberg.org/owner/repo`), surfacing
  `forbidden_host` / `bad_url` as inline errors. The picker itself stays
  GitHub-only (repo *listing* for Gitea is Phase 2).
- `repoRef.ts`: teach the ref helper to carry an optional host so launched
  windows/labels render `codeberg.org/owner/repo` unambiguously next to
  GitHub repos with the same name.

### 6. CLI: clone accepts a URL

`cli/internal/client/api_repos.go` (+ the command that calls it)

- Accept a URL argument where a `full_name` is accepted today; send `{url}`
  when the argument contains `://`, `{full_name}` otherwise. Map the two new
  error codes to actionable messages ("host not allowlisted — ask the operator
  to add it to PROTEOS_GIT_PUBLIC_HOSTS").

### 7. Tests

- **Unit (CP):** `parseCloneURL` table test — happy paths (bare, `.git`,
  trailing slash, `host:port`), rejections (http scheme, userinfo, extra path
  segments, traversal, host not in allowlist, empty allowlist); `parseRemote`
  host extraction; `handleGitClone` handler tests for `url` vs `full_name`
  exclusivity and `forbidden_host`; `handleGitPR` returns `unsupported_host`
  for a non-GitHub remote **without** calling the GitHub client (assert via
  the test double).
- **e2e:** the harness already fakes a git server by overriding `GitHost`
  (`config.go:50-51`). Add a second fake host in `PROTEOS_GIT_PUBLIC_HOSTS`
  and assert: anonymous clone from it succeeds end-to-end (clone → SSE
  `git.clone` event → project listed), credential requests for it are refused,
  and `git pr` against it returns `unsupported_host`.
- **Regression:** full existing suite green with `PROTEOS_GIT_PUBLIC_HOSTS`
  unset — the empty-allowlist deployment must be byte-identical to today.

## Sequencing

1. Config + `parseCloneURL` + clone-by-URL handler (workstreams 1–2, with unit
   tests) — the vertical slice is demoable via `curl` at this point.
2. PR-path host guard (workstream 3) — small, ships with slice 1 in the same
   PR ideally, since slice 1 is what makes the latent bug reachable.
3. Comment/copy sweep (workstream 4).
4. Web + CLI surfaces (workstreams 5–6).
5. e2e second-host coverage (workstream 7).

## Acceptance criteria

- `POST /api/git/clone {"url":"https://codeberg.org/owner/repo"}` on a
  deployment with `PROTEOS_GIT_PUBLIC_HOSTS=codeberg.org` clones the repo into
  `/workspace/repo`; the same request with the host absent from the allowlist
  returns 400 `forbidden_host`; GitHub `full_name` cloning is unchanged.
- Creating a PR on a project whose origin is a public host returns 422
  `unsupported_host` and provably never touches the GitHub API.
- A push from inside the guest to a public-host repo fails with the reworded
  credential-helper message naming the auth-host limitation.
- Existing deployments (no `PROTEOS_GIT_PUBLIC_HOSTS`) are behaviorally
  identical, and the full test suite passes.

## Phase 2 seams this phase deliberately leaves behind

- `parseRemote`'s host return + the `unsupported_host` check in `handleGitPR`
  is the future provider-dispatch point (host → provider client).
- `parseCloneURL` is where per-host provider metadata (API base, auth mode)
  will attach.
- `github_links` untouched; Phase 2 adds a provider dimension (or a
  `git_identities` table) alongside Gitea OAuth2/PAT support, `GET
  /api/v1/repos` listing, `/api/v1` PR creation, and check-runs→commit-status
  mapping for the review surface.
