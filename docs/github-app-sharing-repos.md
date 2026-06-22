# Sharing a repo with the ProteOS GitHub App

ProteOS can only clone (and push to) repositories that the **GitHub App
installation** has been granted access to. This is true even for an org owner:
ownership of the org is not what GitHub checks — the App **installation's
repository selection** is. This guide explains the model and the exact steps to
make a repo show up in the Repos panel.

## How ProteOS decides what you can clone

When you open the Projects launcher or the Settings → GitHub tab, the control
plane calls `GET /api/git/repos`
(`controlplane/internal/httpapi/git.go` → `github.Client.ListUserRepos`), which,
using **your** user-to-server token, asks GitHub:

1. `GET /user/installations` — every App installation you can see.
2. For each, `GET /user/installations/{id}/repositories` — the repos that
   installation can access **and** that you can see.

So a repo appears in ProteOS only when **both** are true:

- the App is **installed** on the account/org that owns the repo, and that
  installation's **repository access** includes the repo (or is set to *All
  repositories*); and
- **your** GitHub account can see that repo (you're a member/collaborator with at
  least read access).

Installing the App on an org is step one; **granting the installation access to
the specific repos is the step that's usually missing.**

## Share a repo owned by an org (e.g. `tavon-ai`)

You need org-owner (or the installation's manager) permissions to do this.

1. Go to the org's installation settings:
   **GitHub → your org → Settings → GitHub Apps → ProteOS → Configure**
   (direct URL: `https://github.com/organizations/<org>/settings/installations`,
   then pick ProteOS).
2. Under **Repository access**, choose either:
   - **All repositories** — every current and future org repo becomes clonable; or
   - **Only select repositories** → **Select repositories** → add the repo(s) you
     want (this is the option that silently excludes repos when left incomplete).
3. Click **Save**.
4. Back in ProteOS, open **Settings → GitHub** (or the **+ Clone repo** panel on
   the Projects launcher) and click **Refresh**. The repo should now appear.

That's it — no re-login is needed. The repo list is fetched live on each call and
the Refresh button refetches it.

## Share a repo owned by your personal account

1. Go to **GitHub → Settings → Applications → Installed GitHub Apps → ProteOS →
   Configure** (direct URL:
   `https://github.com/settings/installations`).
2. Set **Repository access** to **All repositories** or add the specific repos
   under **Only select repositories**, then **Save**.
3. In ProteOS, **Refresh** the Repos panel.

## The "Choose which repositories…" link is missing

The Repos panel and the GitHub settings tab show a **Choose which repositories
ProteOS can access ↗** link that jumps straight to the installation page. It only
renders when the control plane knows the App's slug. The link is built as:

```
https://github.com/apps/<slug>/installations/new
```

from the **`GITHUB_APP_SLUG`** environment variable
(`controlplane/internal/httpapi/git.go` → `grantsURL`). If that variable is
**unset or empty**, `grants_url` comes back empty and the UI omits the link — the
behavior you're seeing. The feature still works; only the convenience link is
hidden.

To restore the link, set `GITHUB_APP_SLUG` to the App's slug and restart the
control plane:

- The slug is the last path segment of the App's public page URL,
  `https://github.com/apps/<slug>` — find it under the App's settings, or in the
  org's installed-apps list.
- Dev/local: add it to `controlplane/.env` (see `controlplane/.env.example`).
- Deployed stack: set it in `deploy/app-stack/.env` (see
  `deploy/app-stack/.env.example`).

```bash
GITHUB_APP_SLUG=proteos        # example; use your App's actual slug
```

Until then, use the direct GitHub URLs in the sections above to reach the
installation settings.

## "Connected" but no repos appear (authorized vs installed)

A GitHub App separates two per-user concepts, and confusing them is the most
common false alarm:

- **Authorization** — shown under your personal **Settings → Applications →
  Authorized GitHub Apps**. It means "ProteOS may act as me." The ProteOS **login**
  creates this. It is *not* a repo grant.
- **Installation** — shown under **Installed GitHub Apps** for the account/org it
  is installed on (e.g. `tavon-ai`). This is the repo grant.

You do **not** need the App installed on your personal account to clone an org's
repos: the **org installation** provides repo access and your **authorization**
provides your identity. If the App is restricted to "only on this account" (the
org), clicking the grants link correctly sends you to the org installation — that
is expected, not a bug.

If ProteOS shows **GitHub connected** (a live token) but the clone panel lists no
repos, the `/user/installations` call is returning **without** the org
installation for your token. Fix it by re-establishing the authorization:

1. **Log out of ProteOS and log back in.** A token *refresh* does not re-link new
   org installations — only a fresh **login** (OAuth re-authorization) does. This
   is the usual fix when the org installation is newer than your last login.
2. Still empty? On **Authorized GitHub Apps → (the ProteOS app)**, check
   **Organization access** for the org. Click **Grant** if offered. If the org
   enforces **SAML SSO**, you must authorize the token for that org here —
   otherwise org installations and repos are invisible to the API even for an
   owner.
3. Confirm you are logged into ProteOS as the GitHub account that is a member of
   the org (check the login shown in **Settings → GitHub**).

## Troubleshooting checklist

If a repo still doesn't appear after granting access and clicking **Refresh**:

- **Right installation.** Confirm the App is installed on the **account that owns
  the repo** (the `tavon-ai` org for an org repo, not your personal account). An
  install on your personal account does **not** grant access to org repos.
- **Repository selected.** In that installation's **Repository access**, confirm
  the repo is in the *select repositories* list (or that it's set to *All
  repositories*).
- **You can see the repo.** Your signed-in GitHub account must have at least read
  access to the repo in the org. The user token only returns repos visible to
  you, intersected with the installation's grants.
- **Signed in as the right user.** ProteOS shows repos for the account you logged
  in with. Check **Settings → GitHub** shows the expected login.
- **Stale grant / revoked token.** If the GitHub tab shows a *reconnect* prompt,
  your authorization expired or was revoked — reconnect, then Refresh.
- **Org third-party policy.** Some orgs restrict app installs; an org owner must
  approve/install ProteOS for the org before any of the above applies.

## Related docs

- `docs/github-app-setup.md` — one-time App creation and control-plane config.
- `docs/github-app.md` — the user-to-server token behaviors the git flows rely on.
