# Plan: Human-in-the-loop git review & GitHub control APIs

> Source: in-conversation design (2026-06-20) — the "mobile agent delegates a task
> to ProteOS" big-picture. After a coding agent (headless or interactive) makes
> changes inside a machine, the owner must be able to **review, test, and only then
> commit / push / open a PR**, all via API so the same flow works from the browser
> desktop and from the mobile agent.
> Off the master-plan 1–12 numbering (feature work, like `port-preview` and
> `multi-machine`); stages labelled **GR1–GR5** to avoid colliding with master-plan
> phase numbers. Layers on top of the **Phase 7** control channel (`guestctl`),
> **Phase 8** machine-web, and the **port-preview** (PP1–PP4) work.

## Scope

Give the **logged-in machine owner** a complete review→publish loop over a repo
cloned in their machine's `/workspace`, exposed as control-plane REST so it is
driveable by both the React desktop and the mobile/agent caller:

1. **Review** — see git status (per-file staged/unstaged/untracked) and a unified diff.
2. **Test** — run the app and reach it via the existing port-preview URL (reused
   as-is; no new work).
3. **Branch** — put work on a feature branch, not on the default branch.
4. **Commit** — stage selected paths and commit, authored as the user.
5. **Push** — push the branch to GitHub over the existing credential helper.
6. **Open PR** — open a pull request from the pushed branch to the default branch.

## Context

This extends the **Phase 7 control channel** rather than inventing a new transport.
Today that single per-machine WebSocket (`controlplane/internal/guestctl/manager.go`,
guest side `guestagent/internal/ctlchan/manager.go`) already carries `git.configure`,
`git.clone`, `git.clone.done`, `git.credential`, `projects.list`, `kv.get`, `kv.set`
using the `ControlFrame{ID,Kind,Op,Payload}` envelope (`guestagent/api/guestwire.go`)
with ID-based req/resp correlation. We add new CP→Guest verbs alongside these; the
framing, dispatch, error shape (`ControlErrorPayload{code,message}`), and reconnect
supervision are all reused.

The two hard problems are already solved:

- **Push auth.** The git credential helper works end-to-end today: a `git push` in the
  guest shells out to `guestagent git-credential`, which hits the local socket
  (`/run/proteos/agent.sock`), which rides the control channel's `git.credential` verb
  back to the CP, which mints a short-lived GitHub token via `TokenSource.Token()`.
  GR4 only needs to *trigger* a push; authentication is automatic.
- **Testing.** Port-preview (PP1–PP4) already lets the owner open
  `https://m-<uuid>-p<port>.<domain>/` for any loopback port. GR1's "test it" step is
  just `POST /api/machine/web-session?port=` — a consumer of an existing feature, no
  new edge work.

What does **not** exist today and is built here: detailed `git status`, `git diff`,
branch create/checkout, a commit verb, a push *trigger* verb, and any GitHub PR-create
API call (the GitHub client only does OAuth + `ListUserRepos` + `GetUser`).

## Architectural decisions

Durable decisions that apply across all stages:

- **Routes — machine-scoped, nested under the machines resource.** All new endpoints
  live under `/api/machines/{id}/git/...`, keeping the per-machine ops with the
  machine they act on (the legacy `/api/git/clone?machine=` and `/api/git/repos` stay
  as-is for back-compat):
  - `GET  /api/machines/{id}/git/status?project=<name>`
  - `GET  /api/machines/{id}/git/diff?project=<name>&staged=<bool>`
  - `POST /api/machines/{id}/git/branch`  `{project, name, checkout, from?}`
  - `POST /api/machines/{id}/git/commit`  `{project, message, paths?}`
  - `POST /api/machines/{id}/git/push`    `{project, branch, set_upstream?}` → 202 + SSE completion
  - `POST /api/machines/{id}/git/pr`       `{project, title, body, base?}` → `{pr_url, number}`
- **Transport — new control-channel verbs.** Add CP→Guest verbs `git.status`,
  `git.diff`, `git.branch`, `git.commit`, `git.push` to `guestwire.go`, each with a
  request/response payload struct, dispatched on the guest by `ctlchan/manager.go`.
  Guest implementations shell out to `git` as the **session user** in the project
  working dir (same pattern as `projects.list` in `ctlchan/projects.go`), never as
  root. PR creation is **not** a guest verb — it's a CP→GitHub API call.
- **PR creation = CP→GitHub, user OAuth token.** A new `GitHub.CreatePR()` method
  (`POST /repos/{owner}/{repo}/pulls`) called with the user's user-to-server token via
  `TokenSource.Token()` — the same auth path as `ListUserRepos`. `owner/repo` is
  derived from the project's `origin` remote (already returned by `projects.list`);
  `base` defaults to the repo's default branch.
- **Project identity & validation.** Every op targets a repo under `/workspace` named
  by `project`, validated exactly like `?cwd=` is today (must be a listable project;
  reject traversal / non-`/workspace` paths). Reuse the existing cwd/project resolver
  so a foreign or non-existent project is a `400 bad_project`, mirroring `bad_cwd`.
- **The review-gate is structural, not a feature flag.** ProteOS has **no code path
  that commits or pushes on the agent's behalf.** A coding agent (headless task lane
  or interactive terminal) only ever produces a dirty working tree; `commit`, `push`,
  and `pr` are always explicit, separately-authorized REST calls. This *is* the
  mechanism that satisfies the customers' "all changes must be human-reviewed before
  commit" policy — there is nothing to bypass because automation stops at the diff.
  (An optional per-repo/org "require_review" hard-block on the headless lane is noted
  as a future extension, not built here.)
- **Auth & safety, consistent with existing mutating endpoints.** All endpoints
  require an authenticated session; mutating ones (`branch`, `commit`, `push`, `pr`)
  require the `X-Requested-By: proteos` CSRF header and that the machine is owned by
  the caller, running, and has a live control channel — the same preconditions
  `handleGitClone` enforces. Every mutating op writes an audit row
  (`ActionGitCommit`, `ActionGitPush`, `ActionGitPRCreate`).
- **Async vs sync.** `status`, `diff`, `branch`, `commit` are synchronous request/
  response over the control channel (fast, local git ops). `push` is potentially slow
  and network-bound, so it follows the `git.clone` pattern: **202 + opID**, with
  completion delivered as a `machine_events` row over the existing SSE stream
  (`git.push` event, success/failure detail sanitized of tokens).
- **No secrets in payloads or logs.** Diffs and status carry file contents but never
  credentials; the credential helper remains the *only* path tokens travel, and push
  detail is sanitized exactly as `git.clone.done` is today.

---

## Phase GR1: Review surface — status & diff

**User stories**: "See exactly what the agent changed before anything is committed";
"test the running app before I decide" (test step reuses port-preview, no new build).

### What to build

A read-only review path end-to-end. New control-channel verbs `git.status` and
`git.diff` that run read-only `git` commands as the session user in the named
project, plus the two `GET` endpoints that expose them. `status` returns structured
per-file entries (path, staged/unstaged/untracked, change type) — richer than the
single `dirty` bool `projects.list` gives today. `diff` returns a unified text diff,
with `?staged=true` selecting the staged (index) diff. The browser desktop gets a
minimal **Changes** panel that lists changed files and renders the diff; the same two
endpoints are what the mobile agent calls to show a review. The "test it" affordance
is wired by reusing `POST /api/machine/web-session?port=` — no new endpoint.

### Acceptance criteria

- [ ] `git.status` / `git.diff` verbs added to the wire protocol with req/resp payload structs and guest-side handlers running as the session user.
- [ ] `GET /api/machines/{id}/git/status?project=` returns per-file change entries; unknown/foreign project → `400 bad_project`; machine not running → `409`; guest unreachable → `502`.
- [ ] `GET /api/machines/{id}/git/diff?project=&staged=` returns a unified diff for the working tree (or the index when `staged=true`).
- [ ] Browser **Changes** panel lists changed files and shows the diff for a selected project.
- [ ] An owner can open the app under test via an existing port-preview URL from the same review view (no new preview code).
- [ ] Unit/integration tests cover the two verbs (clean tree, untracked, staged + unstaged) and the endpoint validation/error mapping.

---

## Phase GR2: Feature branch create / checkout

**User stories**: "Put my work on a feature branch, not on the default branch, so it
follows the PR workflow."

### What to build

A `git.branch` verb that creates and optionally checks out a branch in the named
project, optionally from a given start point (`from`), and the `POST .../git/branch`
endpoint over it. After the call, `git/status` (GR1) reflects the new current branch.
The Changes panel gains a branch indicator + a "new branch" action.

### Acceptance criteria

- [ ] `git.branch` verb added with payload `{project, name, checkout, from?}` and a guest handler that validates the branch name and runs the create/checkout as the session user.
- [ ] `POST /api/machines/{id}/git/branch` creates (and checks out when requested) the branch; invalid branch name → `400`; standard machine/running/channel preconditions enforced with CSRF.
- [ ] `git/status` reports the new branch as current after a checkout.
- [ ] Browser Changes panel shows the current branch and can create+switch to a new one.
- [ ] Tests cover create-only, create+checkout, `from` start point, duplicate-name rejection, and invalid-name rejection.

---

## Phase GR3: Stage & commit

**User stories**: "When I'm happy with the review, commit the changes with a message,
authored as me."

### What to build

A `git.commit` verb that stages the requested paths (or all changes when `paths` is
omitted) and creates a commit with the supplied message, authored with the identity
`git.configure` already wrote to `~/.gitconfig` (the user's GitHub login + email).
The `POST .../git/commit` endpoint exposes it; on success `git/status` goes clean (or
shows only the still-unstaged remainder) and `projects.list` reflects the new HEAD.
The Changes panel gains a commit message box + per-file stage selection. This is the
human gate in action: the commit only happens because the owner explicitly called it.

### Acceptance criteria

- [ ] `git.commit` verb with payload `{project, message, paths?}`; guest handler stages the given paths (or all) and commits as the session user, using the configured author identity; empty message → error; nothing to commit → distinct error code.
- [ ] `POST /api/machines/{id}/git/commit` returns the new commit sha/subject; audited as `ActionGitCommit`; CSRF + ownership/running/channel enforced.
- [ ] After a full-tree commit, `git/status` reports a clean tree and `projects.list` shows the new last-commit message/time.
- [ ] Partial commit: committing a subset of `paths` leaves the rest reported as unstaged.
- [ ] Browser Changes panel supports entering a message, selecting files, and committing.
- [ ] Tests cover full commit, partial-path commit, empty-message rejection, and nothing-to-commit.

---

## Phase GR4: Push to GitHub

**User stories**: "Push the reviewed branch to GitHub."

### What to build

A `git.push` verb that pushes the named branch of the project to `origin`, setting
upstream when `set_upstream` is requested (first push of a new branch). Auth is
automatic — the existing credential helper supplies the token via the `git.credential`
round-trip; this phase adds no new auth. Following the `git.clone` precedent, the
endpoint returns **202 + opID** immediately and the guest reports completion
asynchronously (`git.push.done`-style), recorded as a `machine_events` row and
streamed over the existing SSE channel as a `git.push` event with sanitized detail.
The Changes panel shows push-in-progress → pushed/failed from the SSE stream.

### Acceptance criteria

- [ ] `git.push` verb with payload `{project, branch, set_upstream?}`; guest handler runs `git push` (with `-u` when requested) as the session user, relying on the existing credential helper for auth.
- [ ] `POST /api/machines/{id}/git/push` returns `202 {op_id}`; CSRF + ownership/running/channel enforced; audited as `ActionGitPush`.
- [ ] Completion arrives as a `git.push` `machine_events` row over SSE with success/failure and **token-free** detail.
- [ ] A pushed branch is verifiably present on GitHub (acceptance/integration check); a push to a repo the user hasn't granted fails with a sanitized error surfaced over SSE.
- [ ] Browser Changes panel reflects push progress and terminal state from SSE.
- [ ] Tests cover first push (`set_upstream`), subsequent push, and auth/permission failure mapping.

---

## Phase GR5: Open PR

**User stories**: "Open a pull request from the pushed branch to the default branch."

### What to build

The last hop, a CP→GitHub call (no guest verb). A new `GitHub.CreatePR()` method
(`POST /repos/{owner}/{repo}/pulls`) invoked with the user's OAuth token via
`TokenSource.Token()`. `owner/repo` is derived from the project's `origin` remote
(already in `projects.list`); `head` is the pushed branch; `base` defaults to the
repo's default branch (from `ListUserRepos`/repo metadata) unless overridden. Returns
the PR URL + number, recorded in audit and emitted as an SSE event so the desktop and
the mobile agent both learn the PR landed. This completes the
create→clone→agent→review→test→branch→commit→push→**PR**→(destroy) loop the mobile
agent orchestrates.

### Acceptance criteria

- [ ] `GitHub.CreatePR(ctx, token, owner, repo, head, base, title, body)` implemented against `POST /repos/{owner}/{repo}/pulls`, with GitHub error bodies mapped to clean codes (e.g. no commits between branches, PR already exists, reconnect-needed).
- [ ] `POST /api/machines/{id}/git/pr` derives `owner/repo` from the project's origin remote, defaults `base` to the repo default branch, and returns `{pr_url, number}`; CSRF + ownership enforced; audited as `ActionGitPRCreate`.
- [ ] Revoked/expired GitHub grant → `409 reconnect_github` (same mapping as repo listing); GitHub unavailable → `502 github_unavailable`.
- [ ] An end-to-end run (clone → edit → branch → commit → push → PR) produces a real open PR whose URL is returned and surfaced over SSE.
- [ ] Browser surfaces the PR link on success; the mobile-agent flow receives the same `{pr_url}` from the REST response.
- [ ] Tests cover successful PR creation, no-diff/empty-PR rejection, duplicate-PR handling, and revoked-grant mapping (GitHub client mocked).

---

## Non-goals (this plan)

- **Auto-commit / auto-push by the agent.** Deliberately impossible by design — see the
  review-gate decision. The agent stops at a dirty tree.
- **A hard per-repo/org "require_review" policy block** on the headless task lane.
  The structural gate already enforces human-authored commits; a configurable
  org-policy that additionally forbids the *headless lane* from calling commit/push is
  a clean future extension on top of these endpoints.
- **PR review/merge, comments, status checks, draft PRs.** GR5 opens a PR; managing it
  is GitHub's surface.
- **Conflict resolution / interactive rebase / merge tooling.** Out of scope; the
  human resolves conflicts in the terminal or editor if a push is rejected.
- **A persistent CP-side git/project table.** The guest filesystem stays the source of
  truth (as `projects.list` already establishes); status/diff are read live.
