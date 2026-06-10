# GitHub App setup (Phase 1 auth)

ProteOS authenticates users through a **GitHub App** using its user-authorization
(OAuth) flow — not a classic OAuth app. This gives per-repo installation grants
and short-lived, refreshable user tokens, which the Phase 7 credential broker
needs. The login UX is identical to a classic OAuth app.

This is a one-time manual setup. The control plane reads the resulting
credentials from the environment (see `controlplane/.env.example`).

## 1. Create the App

1. Go to **GitHub → Settings → Developer settings → GitHub Apps → New GitHub App**
   (for an org-owned app, use the org's settings instead).
2. **GitHub App name**: e.g. `ProteOS (dev)` — names are globally unique, so
   suffix per environment.
3. **Homepage URL**: `http://localhost:8080` for dev (any valid URL works).
4. **Callback URL**: add **both** of these to the same App so one App serves dev
   and the deployed host (an App supports multiple callback URLs):
   - `http://localhost:8080/api/auth/github/callback`
   - `https://<your-deployed-host>/api/auth/github/callback`
5. Tick **Request user authorization (OAuth) during installation**.
6. Tick **Opt-in to user-to-server token expiration** — this enables short-lived
   user access tokens **and refresh tokens** (required; Phase 7 refreshes them).
7. **Webhook**: uncheck **Active** (Phase 1 needs no webhooks).
8. **Permissions**: Phase 1 only logs the user in. Set **Account → Email
   addresses: Read-only** if you want the user's email. Repository permissions
   are added in later phases (git operations are Phase 7).
9. **Where can this App be installed?**: *Only on this account* is fine for dev.
10. Click **Create GitHub App**.

## 2. Record the credentials

On the App's page:

- **Client ID** → `GITHUB_APP_CLIENT_ID`
- Click **Generate a new client secret** → `GITHUB_APP_CLIENT_SECRET`
  (shown once — copy it now).

You do **not** need the App's private key (`.pem`) for Phase 1 user login; that
is for app-to-server installation tokens used in later phases.

## 3. Configure the control plane

In `controlplane/.env` (copy from `.env.example`):

```bash
GITHUB_APP_CLIENT_ID=Iv1.xxxxxxxxxxxx
GITHUB_APP_CLIENT_SECRET=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
PROTEOS_BASE_URL=http://localhost:8080
PROTEOS_STATE_KEY=<a long random string, e.g. `openssl rand -hex 32`>
```

`PROTEOS_BASE_URL` must match the host portion of the callback URL you
registered — the control plane builds the callback as
`<PROTEOS_BASE_URL>/api/auth/github/callback`.

## 4. (Optional) signup allowlist

Before any internet-facing deployment, restrict sign-in to invited users:

```bash
ALLOWED_GITHUB_LOGINS=alice,bob
```

Empty (the default) allows any GitHub user. A login not on the list is bounced
back to `/login?error=not_invited`.

## 5. Verify

```bash
docker compose -f compose.dev.yml up -d
cd controlplane && go run ./cmd/controlplane -migrate
# in web/: npm run dev, then open the app and click "Sign in with GitHub"
```

A successful round-trip sets the `proteos_session` cookie and `GET /api/me`
returns your GitHub profile. Tokens are written to the secrets store
(`secret/users/<user_id>/github`), never to Postgres.
