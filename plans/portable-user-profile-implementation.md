# Plan: Portable User Profile (credentials & dotfiles across a user's machines)

> Source: brainstorm 2026-06-22 ("shared workspace between machines"). Status: **not started.**
> Origin pain: spinning up a new machine means re-running the Claude Code subscription
> auth flow (and re-doing git/ssh setup) every time. The state is trapped on one
> machine's per-machine LUKS volume; it should follow the *user*, not the *machine*.
>
> Decision recap from the brainstorm: we are **not** building a live, concurrently-mounted
> shared workspace (single-writer ext4 block devices + cross-fc-node scheduling make that a
> network-FS subsystem we don't want). Instead we make a small set of **user-scoped profile
> items** materialize into each of the user's machines at boot, reusing the Phase 5
> secret-injection path. "Shared workspace" in the literal sense remains a git remote.

## Context

Three repo facts anchor the design (confirmed during the brainstorm):

1. **Provider secrets are already user-scoped.** They live in OpenBao at
   `secret/users/<id>/providers/<key>` and are read by the injector's `compose()`
   (`controlplane/internal/injector/injector.go`), turned into a `SecretsRequest`, and
   `PUT /secrets`'d to the guest. The user child-token policy already scopes
   `secret/users/<uid>/*` (`controlplane/internal/secrets/bao.go`). We add a sibling
   namespace, not a new auth model.
2. **The guest injection path is env-file + tmpfs, replace-all.** `secrets.Store.Replace`
   (`guestagent/internal/secrets/secrets.go`) writes one `0600` sourceable env file per
   provider entry under a tmpfs dir, login shells source them via a `profile.d` snippet,
   and agent sessions overlay the same env. Secrets never touch the rootfs or the persist
   disk. A push that drops an entry deletes its file (lockstep).
3. **Injection is keyed by `(userID, machineID)` with ownership already enforced.** The
   poller injects on machine→running (`controlplane/internal/machine/poller.go`), the agent
   route injects before a session, and a machine only ever receives its owner's secrets.

The generalization that makes this not-Claude-only: a `ProviderDef` carries an `Env` map
and is written verbatim to a sourceable file with **no requirement that it have a launch
command**. So an `env`-kind profile item is just a synthetic, non-launch entry in the
existing `SecretsRequest.Providers` map — **no guestagent change is needed for Tier 0.**
`file`-kind items (gitconfig, ssh) genuinely need a new guest primitive and are deferred to
Phases 3–4.

## Architectural decisions

Durable decisions that apply across all phases:

- **Storage**: OpenBao, user-scoped, at `secret/users/<id>/profile/<key>` — one secret per
  profile item, distinct from `.../providers/<key>`. Reuses the existing `user-<uid>` Bao
  policy and child-token minting; no new policy shape. Values live **only** in OpenBao.
- **Profile item model**: each item is `{ key, kind, target, value }`.
  - `kind: "env"` → `target` is an environment variable name. (Tier 0.)
  - `kind: "file"` → `target` is `{ path, mode }` relative to `$HOME`. (Phase 3+.)
  - The model is generic from day one; Tier 0 implements only `kind: "env"`.
- **Injection** (refined after the v2.1.169 auth verification — see below):
  - **Terminal vs. agent env is not the same surface.** Login shells source *every*
    provider's tmpfs env file, but an agent session overlays only the *launched* provider's
    env (`secrets.EnvList(key)`). So an env var a provider needs to authenticate (e.g.
    `CLAUDE_CODE_OAUTH_TOKEN`) must be composed into **that provider's** `Env`, not a
    standalone entry — otherwise it reaches the terminal but not an agent-launched `claude`.
  - Therefore: an `env`-kind profile item that is a **provider auth credential** is merged
    into the target provider's `ProviderDef.Env` during `injector.compose()`. A generic
    `env` item not tied to a provider may still ride a synthetic non-launch entry (reserved
    key, e.g. `__profile`) for login-shell sourcing — but that path is login-shell-only.
  - `file`-kind items require a new guest materializer (Phase 3) — out of scope until then.
- **Routes**: RESTful under the authenticated user.
  - `GET    /api/profile/items` — list items (metadata only; never returns secret values).
  - `PUT    /api/profile/items/<key>` — create/replace an item's value.
  - `DELETE /api/profile/items/<key>` — remove an item (stops propagation).
  - Phase 4 may add typed conveniences (`/api/profile/git`, `/api/profile/ssh`) over the
    same store.
- **Postgres**: holds only **presence/metadata** per item (`key`, `kind`, `created_at`,
  `updated_at`, optional `expires_at`/status). Never the secret value. Drives UI status and
  lets the injector know what to fetch without listing OpenBao.
- **Consent & ownership**: items are set by an explicit authenticated user action; injection
  reuses the existing owner-scoped `(userID, machineID)` path, so an item only ever reaches
  its owner's machines.
- **Security posture**: values OpenBao-only, redacted in all logs, excluded from any debug
  dumps; `DELETE` stops *propagation* (true credential revocation is the upstream provider's
  concern and is surfaced in copy, not implied).

### Non-goals (explicitly deferred)

- **Tier 1 auto-capture** of the interactive `claude` login (fsnotify watcher in the guest +
  a guest→CP control-channel sync op + OAuth refresh-token rotation handling across
  concurrently-running machines). Tier 0's static `claude setup-token` token avoids the
  rotation race entirely; revisit only if the manual paste proves annoying.
- **Live, concurrently-mounted shared `/workspace`** across running machines.
- **Arbitrary dotfile bundles** beyond gitconfig/ssh (a later, separate plan once the
  `file`-kind primitive exists).

### Verified against Claude Code v2.1.169 (2026-06-22)

Checked against the official docs (`code.claude.com/docs/en/authentication.md`,
`.../env-vars.md`) and the installed CLI (2.1.185; bake pins 2.1.169):

- ✅ **`CLAUDE_CODE_OAUTH_TOKEN` is supported** for Pro/Max/Team/Enterprise subscription auth,
  minted by **`claude setup-token`** (a **one-year** token; generating it needs a one-time
  interactive OAuth, after which it is just a string we store). `setup-token` is present in
  the installed 2.1.x; the feature predates 2.1.169.
- ✅ **No precedence collision in practice.** Auth precedence puts `ANTHROPIC_API_KEY` (#3)
  *above* `CLAUDE_CODE_OAUTH_TOKEN` (#5). The injector only emits `ANTHROPIC_API_KEY` for
  users with a stored API key; the OAuth token only matters for *subscription/keyless* users
  who have no API key. **Constraint:** inject the token in the keyless/subscription branch
  (`injector.go:109`) and never emit an empty `ANTHROPIC_API_KEY` alongside it.
- ✅ **Launch command is plain `claude`** (no `--bare`), so the "`--bare` ignores
  `CLAUDE_CODE_OAUTH_TOKEN`" caveat does not apply.
- ⚠️ **One residual unknown (~80% doc confidence): interactive `claude` (no `-p`) honoring the
  token with zero prompts.** Docs place it above subscription-login creds in precedence but
  don't state interactive behavior explicitly. **This is the one thing to smoke-test on a
  booted VM running the baked 2.1.169** — and it is already a Phase 1 acceptance criterion.
- Token TTL is one year → the "needs reconnect" handling (Phase 2) keys off an `expires_at`
  ~1 year out.

---

## Phase 1: Generic profile store + Claude token (backend tracer bullet) — **landed (backend, CP tests green)**

> Implemented 2026-06-23. Backend tracer bullet complete and unit/integration-tested
> against a Testcontainers Postgres. The two acceptance criteria that require a booted
> VM running the pinned Claude Code (`CLAUDE_CODE_OAUTH_TOKEN` present in shell + agent
> env; `claude` starts authenticated with no interactive login) remain **operator-pending**
> on-machine smoke tests. What shipped:
>
> - Migration `000011_profile_items` (metadata only: `key, kind, target, expires_at,
>   created/updated_at`, PK `(user_id, key)`, FK→users `ON DELETE CASCADE`). Values live
>   only in OpenBao at `secret/users/<id>/profile/<key>` (`secrets.UserProfilePath`),
>   covered by the existing `user-<id>` policy — no policy change.
> - New `internal/profile` package: a server-side `Def` registry (Phase 1 ships exactly
>   `claude-oauth` → env `CLAUDE_CODE_OAUTH_TOKEN`, provider `claude`, 1y TTL) and a
>   `Store` combining Postgres metadata + OpenBao values (value-first write; reads audited).
> - `injector.compose()` resolves the user's env-kind profile items and merges
>   provider-tied credentials into **that provider's** `ProviderDef.Env` in the
>   keyless/subscription branch — so the token reaches both login shells and agent
>   sessions — and never emits an empty `ANTHROPIC_API_KEY` alongside it. A stored API key
>   still wins (token is not injected in the keyed branch; covered by a test).
> - Routes (auth + CSRF on mutations, owner-scoped, no value-read route):
>   `GET /api/profile/items`, `PUT|DELETE /api/profile/items/{key}`. Unknown key → 404;
>   empty/oversized value → 422.
> - Tests: injector merge + API-key precedence; API lifecycle (store→list→delete),
>   metadata-only list, no-value-in-Postgres/response/audit, 404/422/CSRF guards.
> - No guestagent code changed (criterion met).

### (original plan below)


**User stories**: As a user, after I register my Claude subscription token once, every new
machine I create launches with `claude` already authenticated — no per-machine login flow.

### What to build

The profile abstraction, end-to-end, with the Claude OAuth token as its first and only
item — proving the whole path through every layer without any guestagent change.

- Persist a profile item value to OpenBao at `secret/users/<id>/profile/<key>` and a
  metadata row in Postgres, set/cleared through `PUT/DELETE /api/profile/items/<key>`.
- Teach `injector.compose()` to read the user's `env`-kind profile items. The Claude OAuth
  token (target `CLAUDE_CODE_OAUTH_TOKEN`) is composed into the **`claude` provider's `Env`**
  in the keyless/subscription branch (`injector.go:109`) — so it reaches both terminal and
  agent sessions — and **no empty `ANTHROPIC_API_KEY` is emitted** alongside it.
- Register the Claude OAuth token as an `env` item targeting `CLAUDE_CODE_OAUTH_TOKEN`.
- A newly created machine, on reaching running, receives the push; the token is present in
  the login shell and to agent sessions; `claude` runs authenticated.

This phase may be API-only (no polished UI) — it is the tracer bullet, verified via the API
and on-machine, with the product surface following in Phase 2.

### Acceptance criteria

- [ ] `PUT /api/profile/items/claude-oauth` with a token value stores it at
      `secret/users/<id>/profile/claude-oauth` in OpenBao and writes a metadata row; the
      value is never written to Postgres or any log.
- [ ] `GET /api/profile/items` returns the item's metadata (key, kind, timestamps) and
      **never** the secret value.
- [ ] A machine created *after* the item is set has `CLAUDE_CODE_OAUTH_TOKEN` present in an
      interactive login shell and in an agent session's environment.
- [ ] In that machine, `claude` starts authenticated against the subscription with **no**
      interactive login (verified against the pinned Claude Code version).
- [ ] `DELETE /api/profile/items/claude-oauth` removes the OpenBao entry and metadata row;
      the next push to a machine drops the env file (replace-all lockstep), so the var is
      gone after the machine's next injection.
- [ ] No guestagent code changed in this phase.
- [ ] Injection remains owner-scoped: a machine never receives another user's profile items.

---

## Phase 2: Claude subscription product surface

**User stories**: As a user, I can connect and disconnect my Claude subscription from the UI,
see whether it's connected, and have an already-running machine pick up a newly connected
token without recreating it.

### What to build

Wrap Phase 1's backend in a usable, safe product surface and close the lifecycle gaps.

- A "Connect Claude subscription" UI: paste the `claude setup-token` output, show connection
  status, and disconnect. Brief inline guidance on generating the token and on what
  disconnect does (stops propagation; upstream revocation is separate).
- Surface a "needs reconnect" status when the stored token is known-expired (from metadata)
  or reported invalid.
- **Re-inject to already-running machines** on connect/disconnect, not just on next boot, so
  the change takes effect without recreating a machine.
- Logging redaction audit across the injector/API/Bao paths and a focused security review of
  the new credential surface.

### Acceptance criteria

- [ ] A user can connect a token, see "Connected", and disconnect from the UI.
- [ ] Connecting a token causes all of the user's currently-running machines to receive the
      token on the next injection cycle (not only newly created ones).
- [ ] Disconnecting removes the token from running machines on the next injection cycle.
- [ ] An expired/invalid token surfaces a "needs reconnect" state rather than silently
      failing.
- [ ] A security pass confirms the token value never appears in logs, error messages, API
      list responses, or Postgres, and that the route is authenticated and owner-scoped.
- [ ] Copy makes clear that disconnect stops propagation, not upstream revocation.

---

## Phase 3: File-kind profile items (the guest materializer)

**User stories**: As a user, a file I register in my profile appears at the right path in my
home directory on every machine, with the right permissions — proving the `file` kind
end-to-end with one trivial item.

### What to build

The new guest primitive that materializes `file`-kind profile items into `$HOME`, kept
deliberately generic and proven with a single trivial dotfile (not yet gitconfig/ssh).

- Extend the secret push so the guest can write a profile item's value to a `$HOME`-relative
  path with an explicit mode and the session user's ownership. (Mechanism choice — extend
  the `SecretsRequest` schema with a files section vs. reuse a Phase 6 `setup_command`
  writer — is an implementation detail to settle at build time; either way the value stays
  off the rootfs and is written under the persist-disk `$HOME`.)
- `injector.compose()` emits `file`-kind items alongside the `env` synthetic provider.
- Tracer item: an arbitrary dotfile set via `PUT /api/profile/items/<key>` with
  `kind: "file"` appears on a freshly created machine at the requested path and mode.

### Acceptance criteria

- [ ] A `file`-kind profile item set via the API appears at its `$HOME`-relative target path
      on a newly created machine, with the requested mode and owned by the session user.
- [ ] The item's value is not present on the rootfs image and not in any log.
- [ ] Removing the item via `DELETE` removes (or stops re-creating) the file on the next
      injection, consistent with replace-all semantics.
- [ ] `env`-kind items (Phase 1) continue to work unchanged alongside `file`-kind items.
- [ ] Guest changes are covered by tests for path/mode/ownership and the replace-all drop.

---

## Phase 4: Git identity + SSH key as profile items

**User stories**: As a user, my git identity (`~/.gitconfig`) and my SSH key (`~/.ssh`) are
present on every machine automatically, so I can commit with the right identity and push/pull
over SSH without per-machine setup.

### What to build

Typed, first-class profile items for the two most-wanted files, built on Phase 3's
materializer.

- **Git identity**: structured name/email (plus optional extra config) rendered to
  `~/.gitconfig`. Consider reconciling with the existing Phase 7 git-credential/identity
  wiring so the two don't fight over identity.
- **SSH key**: generate-in-platform or import an existing keypair; store the private key as a
  `file`-kind item written `0600` to `~/.ssh/`, set `~/.ssh` to `0700`, and surface the
  **public** key in the UI for the user to add to GitHub/etc. Handle `known_hosts` seeding as
  appropriate.
- A security pass specific to private-key handling (storage, transport, perms, redaction,
  and what "disconnect" means for a key already added to a remote).

### Acceptance criteria

- [ ] Setting git identity makes `~/.gitconfig` reflect the configured name/email on a new
      machine, and `git commit` uses that identity.
- [ ] An SSH key registered in the profile appears at `~/.ssh/<key>` with `0600` and `~/.ssh`
      at `0700` on a new machine, and an SSH git remote operation succeeds using it.
- [ ] The public key is retrievable from the UI; the private key is never returned by any API
      or written to logs/Postgres.
- [ ] Git identity from the profile coexists with (does not double-configure or conflict
      with) the Phase 7 git-credential path.
- [ ] A security review signs off on private-key storage, transport, permissions, and the
      disconnect semantics.

---

## Suggested sequencing notes

- Phases 1–2 are **Tier 0** and independently shippable: after Phase 2, the original pain
  ("re-auth Claude on every new machine") is solved for users.
- Phases 3–4 are the **generic extension** the model was designed for; Phase 3 carries the
  one-time guest cost, after which Phase 4 (and any future dotfile work) is additive.
- Revisit Tier 1 auto-capture only if the manual `setup-token` paste proves to be real
  friction; it trades the manual step for a watcher + rotation-race handling.
