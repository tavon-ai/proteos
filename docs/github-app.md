# GitHub App facts (Phase 7)

This pins the GitHub-App behaviours the Phase 7 git contracts depend on
(`plans/phase-7-implementation.md`). The control-plane code encodes these
assumptions; if a live check below contradicts one, fix the code **and** this
doc together.

> Verification status. The shapes here are GitHub's documented App
> user-to-server behaviour and the basis of the implementation + the in-repo e2e
> harness (`guestagent/cmd/guestagent/gitcredential_e2e_test.go`,
> `controlplane/internal/github/tokensource_test.go`). The **[verify live]**
> items were exercised end-to-end during the **task 7.6 live acceptance pass on
> the Proxmox stack with the real GitHub App (green 2026-06-15)** — repo listing,
> clone of a real private repo, commit, push, the revoke→reconnect drill, and a
> stop/start mid-session all behaved as the contracts assume. They remain tagged
> below as the things to re-check if the App configuration changes.

## 1. Expiring user tokens are enabled

The dev App must have **"Expiring user authorization tokens"** turned **on**
(App settings → Optional features). With it on, the OAuth code exchange returns
both an `access_token` (~8 h) and a `refresh_token` (~6 months), plus their
`expires_in` / `refresh_token_expires_in`. Phase 1 already stores all four
(`internal/auth/auth.go`); Phase 7 adds the absolute expiry timestamps.

**[verify live]** Confirm the App setting is on and the callback response
carries `refresh_token` + both `*_expires_in` fields.

## 2. Refresh rotates the refresh token

`POST https://github.com/login/oauth/access_token`

```
grant_type=refresh_token
refresh_token=<current>
client_id=<id>
client_secret=<secret>
```

→ `200` with a **new** `access_token` **and a new `refresh_token`** (GitHub
rotates the refresh token on every use) plus fresh `expires_in` /
`refresh_token_expires_in`. A dead/already-used refresh token returns `200` with
an error body `{"error":"bad_refresh_token"}`.

Implementation: `github.Client.Refresh` (maps `bad_refresh_token` →
`ErrBadRefreshToken`). The `TokenSource` **persists the rotated pair before
releasing the access token** — releasing first and crashing would strand the
user with a dead refresh token (`internal/github/tokensource.go`, decision #4).

**[verify live]** Refresh once, confirm the returned `refresh_token` differs
from the one sent, and that reusing the old one yields `bad_refresh_token`.

## 3. git-over-HTTPS username for user-to-server tokens

Clone/fetch/push over HTTPS with a user-to-server token use HTTP Basic auth with
username **`x-access-token`** and the token as the password:

```
https://x-access-token:<token>@github.com/<owner>/<repo>.git
```

ProteOS never embeds the token in the URL — the credential helper returns
`username=x-access-token` / `password=<token>` on demand (decisions #5, #7). The
e2e harness asserts exactly this Basic-auth shape.

**[verify live]** A `git push` with `x-access-token:<token>` Basic auth to a
granted private repo succeeds.

## 4. Listing installed-and-accessible repos (user-to-server, paginated)

With a GitHub App, the **user chooses** which repos the App may access, so the
authoritative "what can I clone" set comes from the user's installations:

```
GET https://api.github.com/user/installations?per_page=100
  → { "total_count": N, "installations": [ { "id": <installation_id>, … } ] }

GET https://api.github.com/user/installations/<installation_id>/repositories?per_page=100&page=<n>
  → { "total_count": N, "repositories": [ { "full_name", "private",
                                           "default_branch", "pushed_at", … } ] }
```

Implementation: `github.Client.ListUserRepos` iterates installations, paginates
repositories, de-duplicates by `full_name`, and sorts by `pushed_at` (newest
first). `GET /api/git/repos` returns this set plus `grants_url`.

**[verify live]** Confirm both endpoints work with the user token and that a repo
toggled off in the App's installation settings disappears from the list.

## 5. Email captured at login + push attribution

Phase 1 stores `users.email` from `GET /user` (`internal/auth/auth.go`). If the
account hides its email, `GET /user` returns `null`; Phase 7's `git.configure`
falls back to the GitHub no-reply form `"<login>@users.noreply.github.com"`
(`internal/guestctl/manager.go`). `user.name` = the GitHub login.

**[verify live]** Confirm a commit pushed from a machine is attributed to the
user on github.com (i.e. the captured/noreply email maps to the account). If
push attribution needs the `id+login@users.noreply.github.com` numeric form,
adjust the fallback.

## 6. Installation-management ("grants") URL

The Repos panel links the user to choose which repos the App can see:

```
https://github.com/apps/<slug>/installations/new
```

`<slug>` is the App's URL slug, configured as `GITHUB_APP_SLUG`
(`controlplane/internal/config`). When unset the UI omits the links.

**[verify live]** Confirm the slug and that the URL lands on the App's
repo-selection page for the signed-in user.

## Config summary (control plane)

| Env | Meaning |
|-----|---------|
| `GITHUB_APP_CLIENT_ID` / `GITHUB_APP_CLIENT_SECRET` | App user-auth credentials (Phase 1) |
| `GITHUB_APP_SLUG` | builds the grants URL (`github.com/apps/<slug>`) |
| `PROTEOS_GIT_HOST` | host clones target + the credential handler mints for (default `github.com`; e2e override only) |
