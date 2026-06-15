# Phase 7 Implementation Plan: GitHub git operations

> Source: `plans/proteos-poc-to-prod.md` Phase 7, planned 2026-06-11.
> Status: **COMPLETE — landed 2026-06-15.** Backend (control channel
> `internal/guestctl`, `github.TokenSource`, repos/clone APIs), guest
> (`internal/ctlchan`, `internal/localsock`, `guestagent git-credential`), git
> baked into the rootfs (`build-rootfs.sh` download+extract-only + ansible +
> `verify-phase7-rootfs.sh`), the React Repos panel + GitHub status, and the CI
> e2e (real git clone/commit/push through the helper, token-never-on-disk) are
> implemented and tested. Task 7.6's **live acceptance on the Proxmox stack with
> the real GitHub App passed 2026-06-15** (repo list, clone of a real private
> repo, commit, push; revoke→reconnect drill; stop/start mid-session), which also
> confirmed the `docs/github-app.md` facts.
> Prerequisites: Phases 1–3 landed — that is enough for everything here. Phase 5 (OpenBao)
> and Phase 4 (persistent disk) are planned but not landed; Phase 7 needs neither to be
> implementable: tokens go through the `secrets.Store` **interface** (FileStore today,
> OpenBao when Phase 5 lands — zero call-site changes), and clones land in `/workspace`
> whose *durability* is Phase 4's deliverable, not this phase's. The audit-log table is
> introduced by Phase 5's migration; if Phase 7 lands first, create it here and annotate
> Phase 5's plan (same fold-forward rule as Phase 6's header).

## Context

Phase 1 already made the load-bearing decision: it is a **GitHub App with expiring user
tokens** — the callback stores `access_token`, `refresh_token`, `expires_in`,
`refresh_token_expires_in` through `secrets.Store` (`internal/auth/auth.go:154`), and
`github_links` holds only a `secret_ref` + metadata. What's missing is everything that
*uses* the grant: token refresh, a repo list, clone, and — the structurally new piece —
**git credentials inside the VM**.

That last piece is the first time anything *inside* the guest needs to ask the control
plane for something on demand (`git push` invokes the credential helper at arbitrary
times; App user tokens last ~8 h and installation-scoped tokens ~1 h, so pre-baking a
credential is exactly what the acceptance criteria forbid). Until now every
control-plane↔guest interaction has been control-plane-initiated (Phase 3 terminal dial,
Phase 5 secret push). Two facts shape the solution:

1. **The control plane can already dial any running guest** over the vsock tunnel
   (`nodeclient.DialGuest`), and that channel's identity is attested by topology — the
   per-jail unix socket *is* the machine identity, no app-layer credential needed
   (Phase 3 decision #10).
2. **The guest is untrusted but acts with exactly the owner's authority** (master-plan
   trust model). Any process in the VM may request a git credential; that is fine *by
   design*, because the credential it gets is the owner's own token, scoped to repos the
   owner granted the App — never more.

So: keep the guest a pure server. The control plane maintains a **persistent control
channel** to each running guest (CP-dialed, so no guest-initiated transport exists and
the per-machine identity stays unnecessary — see decision #2), and the credential helper's
on-demand requests travel guest→CP *over that CP-dialed channel*. The same channel
carries git setup and clone commands, and is deliberately the seam Phase 11's
guest-activity reporting ("a machine running a long AI-agent task is not idle") will
reuse.

## Architecture decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | **Persistent guest control channel.** New control-plane `internal/guestctl`: a manager that watches machine state (poller/broker events) and maintains one WS per `running` machine — dialed through the existing node-agent tunnel to a new guest endpoint `GET /control` — with backoff reconnect, teardown on stop/hibernate, and re-dial on resume (in-flight conns die on resume per the security baseline; the manager treats that as a normal reconnect). Frames are JSON request/response pairs with ids, **bidirectional**: CP→guest commands (`git.configure`, `git.clone`) and guest→CP requests (`git.credential`). | On-demand guest→CP requests without ever letting the guest initiate a connection — the channel inherits the vsock topology-attested identity, and "never give the guest channel broader power than the user" is enforced at one choke point (decision #3). Phase 5's one-shot `PUT /secrets` push stays as-is (no cross-plan churn); unifying it onto this channel is a later cleanup. Phase 11's idle/activity reporting gets its transport for free. |
| 2 | **Per-machine identity (`secret/machines/<id>/identity`) is *still* not minted.** Phase 5's plan (decision #8) predicted Phase 7's credential helper would be its first consumer — that turned out false under the CP-dialed channel: there is still no guest-initiated transport to authenticate. Defer until one exists (tap+mTLS fallback, or a transport change in Phase 11's cross-host routing); update Phase 5's decision-#8 rationale and the guestagent README note. | Minting an identity nothing consumes was the argument in Phase 5; it still holds. The trust-model section's "authenticates with the per-machine identity" describes the *guest-initiated* variant the topology has so far avoided — record that explicitly so the deviation is a decision, not drift. |
| 3 | **Every channel request is authorized at one choke point**: machine id (from the dial, not from the payload) → owner user → allowed operation set. Phase 7's set is exactly `git.credential`; the handler validates `host == github.com` and `protocol == https` (config-overridable host for the e2e harness only), resolves the owner's token via the TokenSource, audits, and returns `{username, password, expiry}`. Anything else → error frame. | The untrusted-guest rule made executable: requests carry only the owning user's authority, enumerated operations only. Adding Phase 11 activity reports later = adding one handler, same authz spine. |
| 4 | **`github.TokenSource` — per-user token lifecycle in the control plane**: returns a valid user access token; refreshes when <10 min remain (GitHub **rotates the refresh token on every use** — persist the new pair to `secrets.Store` *before* releasing the token; per-user singleflight lock; single CP instance today, PG advisory lock noted for Phase 11 multi-instance). Refresh failure with `bad_refresh_token`/revocation → mark `github_links.metadata.revoked=true` → all git ops return `409 reconnect_github` until the user re-runs the login flow (which re-links and clears the flag). | "Token refresh/expiry is handled; a revoked grant cleanly fails" as one component with one failure mode, used identically by `/api/git/repos` and the credential handler. Rotation-before-release matters: releasing first and crashing strands users with a dead refresh token. |
| 5 | **Credential helper = `guestagent git-credential` subcommand** (same static binary already in the rootfs, busybox-style), wired by gitconfig. It speaks the standard git credential protocol on stdio, forwards only the `get` action to the guest agent over a new **local** unix socket `/run/proteos/agent.sock` (tmpfs), which relays over the control channel; `store`/`erase` are no-ops. Response includes `password_expiry_utc` so git knows the credential ages. The guest agent may cache a credential **in memory ≤60 s** (one `git fetch` invokes the helper several times); nothing token-shaped ever touches disk in the VM. | Satisfies "fetched on demand, short-lived, never written to disk" literally. Reusing the guestagent binary means no new image artifact and no PATH games; the local socket keeps the helper dumb (no WS client in a subprocess). |
| 6 | **Git identity and config are pushed, not baked**: on every channel connect (start *and* resume), CP sends `git.configure` → guest writes `~/.gitconfig`: `user.name` = GitHub login (or display name), `user.email` = the account's primary/noreply email (captured at login; 7.0 verifies what Phase 1 stored), `credential.helper = /usr/local/bin/guestagent git-credential`, `credential.useHttpPath = false`, `safe.directory = /workspace/*`. The file contains **no secret** — persisting it on the (future) Phase 4 home disk is fine. | "git commit uses the user's GitHub identity" with zero interactive setup, idempotent on every boot shape, and consistent with the push-configuration pattern from Phase 5. |
| 7 | **Repos + clone APIs (durable shapes from the master plan)**: `GET /api/git/repos` → TokenSource → GitHub user-accessible-installed-repos listing (user-to-server; paginated; sorted by `pushed_at`), plus `grants_url` (the App's installation-settings URL) in the response envelope — with a GitHub App, *the user controls which repos the app can see*, and the UI must link to that management page rather than pretend all repos exist. `POST /api/git/clone {"full_name":"owner/repo"}` → validate the repo appears in the listable set → channel `git.clone` → guest runs `git clone https://github.com/owner/repo.git /workspace/<repo>` (credential comes from the helper at fetch time — the URL **never embeds a token**) → immediate `202 {op_id}`; completion/failure lands as a `machine_events` info row (`type: git.clone`) → existing SSE → UI. Audit rows (Phase 5 table): `git.repos`, `git.clone`, `git.credential` (target = repo/host, never token material). | Async-with-event clone reuses the SSE pipeline instead of inventing a job API; validating clone targets against the listable set keeps the control plane from being a clone-anything proxy; token-in-helper (not in remote URL) is what makes "no token persisted in plaintext" true — `.git/config` keeps the clean URL. |
| 8 | **React**: Dashboard gains a Repos panel — list (name, private badge, pushed_at), clone button with progress→done via the events stream, empty state and footer both linking to `grants_url` ("choose which repos ProteOS can access"), and a "GitHub connection" status chip that turns into a **Reconnect** banner on `409 reconnect_github`. | The App-grant UX is the one place GitHub-App auth is user-visibly different from classic OAuth; surfacing it beats support tickets about "missing repos". |

## Wire contracts

### Control channel (guestwire additions; CP-dialed WS at guest `GET /control`)

```
frame: {"id":N,"kind":"req|resp|err","op":"…","payload":{…}}

CP → guest:
  git.configure  {"name":"Ivan Pedrazas","email":"…","helper":"/usr/local/bin/guestagent git-credential"}
  git.clone      {"url":"https://github.com/owner/repo.git","dest":"/workspace/repo","op_id":"…"}
                 → immediate ack; later guest→CP event frame:
                   git.clone.done {"op_id":"…","ok":true|false,"detail":"…"}
guest → CP:
  git.credential {"host":"github.com","protocol":"https"}
                 → resp {"username":"x-access-token","password":"<token>","expiry":"<RFC3339>"}
                 → err  {"code":"reconnect_github"|"forbidden_host"|"unavailable"}
exactly-one-channel-per-machine; reconnect = re-send git.configure (idempotent)
```

### Credential helper (in-VM)

```
~/.gitconfig:  credential.helper = /usr/local/bin/guestagent git-credential
git → helper stdio (credential protocol): action=get, host=github.com, protocol=https
helper → guest agent over /run/proteos/agent.sock → control channel (above)
helper stdout: username=…/password=…/password_expiry_utc=…   (store/erase: no-op)
guest agent in-memory cache ≤60s; no disk writes anywhere in the VM
```

### Control-plane API (new)

```
GET  /api/git/repos          → 200 {"repos":[{"full_name","private","default_branch","pushed_at"}…],
                                    "grants_url":"https://github.com/apps/<slug>/installations/new"}
                               · 409 reconnect_github
POST /api/git/clone          body {"full_name":"owner/repo"} → 202 {"op_id":…}
                               · 404 not in listable set · 409 machine_not_running / reconnect_github
                               completion via /api/machine/events (type git.clone)
```

### Config additions

```
controlplane: GITHUB_APP_SLUG (for grants_url),
              PROTEOS_GIT_HOST=github.com   # override only for the e2e harness's local git server
```

## Package layout (new / touched)

```
controlplane/
  internal/github/client.go        # + Refresh, ListUserRepos (user-to-server, paginated), token types
  internal/github/tokensource.go   # NEW: refresh-before-expiry, rotation-safe persist, singleflight,
                                   #      revoked marking (github_links.metadata)
  internal/guestctl/               # NEW: channel manager (state-watch, dial, reconnect), frame types,
                                   #      authz choke point + op handlers (git.credential)
  internal/httpapi/git.go          # NEW: /api/git/repos, /api/git/clone
guestagent/
  cmd/guestagent/                  # git-credential subcommand (credential-protocol stdio client)
  internal/ctlchan/                # NEW: /control WS server, frame dispatch, git.configure/clone exec,
                                   #      credential relay + 60s memory cache
  internal/localsock/              # NEW: /run/proteos/agent.sock server for the helper
  api/                             # control-channel frame types
web/src/
  components/ReposPanel.tsx        # NEW: list, clone w/ event-driven progress, grants_url links
  components/GitHubStatus.tsx      # NEW: connection chip / reconnect banner
  api/client.ts                    # types + endpoints
```

No new Postgres migration: `github_links.metadata` (jsonb, already exists) carries
`revoked` + token-expiry hints; clone completions are `machine_events` rows; the audit
table comes from Phase 5's migration (or moves here per the header note).

## Tasks (Track A = Mac/dev-driver; Track B = Proxmox/Firecracker)

### 7.0 — GitHub App facts + dev-app configuration (Track A; standalone, do first)
Pin the facts the contracts above assume, against the real GitHub App used in dev:
"expiring user tokens" enabled on the App; refresh-token rotation behavior; the exact
git-over-HTTPS username convention for user-to-server tokens (`x-access-token` vs other);
the correct user-to-server endpoint for listing *installed-and-accessible* repos
(+ pagination); what email Phase 1 captured (primary vs noreply) and whether `git push`
attribution works with it; the installation-management URL shape. Record in
`docs/github-app.md`; correct this plan's contracts if any assumption fails.
**Done when:** each fact is demonstrated with curl against GitHub using the dev App and
written down with the request/response shapes.

### 7.1 — Control channel: guest `/control` + CP manager (Track A)
`guestwire` frame types; guest `internal/ctlchan` (WS server on the existing listener,
dispatch, `git.configure` writes gitconfig idempotently); CP `internal/guestctl`
(state-watch via broker, dial over `DialGuest`, reconnect with backoff, teardown on
stop, authz choke point per decision #3 with `git.credential` initially stubbed).
**Done when:** dev-stack e2e: machine reaches running → channel up → `git.configure`
applied in the guest; stop tears it down; restart re-establishes and re-configures;
an unknown op from the guest gets an error frame and a log line.

### 7.2 — TokenSource + refresh + revocation (Track A; parallel with 7.1)
`internal/github` additions per decision #4, tested against the Phase 1 fake-GitHub
pattern: refresh-before-expiry, **rotated refresh token persisted before release**,
concurrent callers singleflighted, `bad_refresh_token` → revoked marking → typed error;
re-login clears revoked.
**Done when:** table-driven tests cover fresh/expiring/rotated/revoked/concurrent; no
token material in logs (scan test, same discipline as Phases 4–6).

### 7.3 — Credential helper end-to-end in the guest (Track A; after 7.1 + 7.2)
`/run/proteos/agent.sock` local server; `guestagent git-credential` subcommand
(credential protocol, `get` only); relay over the channel; CP `git.credential` handler
(host/protocol validation, TokenSource, audit row); ≤60 s in-memory cache; expiry passed
through.
**Done when:** inside a dev-stack guest, `git credential fill` returns a token-shaped
credential with expiry; revoked grant → helper exits non-zero with `reconnect_github` on
stderr; `grep -r` of the guest filesystem after operations finds no token material.

### 7.4 — Repos + clone APIs (Track A; after 7.2, clone exec after 7.1)
`httpapi/git.go` per decision #7; clone validation against the listable set; `git.clone`
execution in the guest (async, op_id, completion event → `machine_events` → SSE); audit
rows.
**Done when:** handler tests cover 200/202/404/409 matrix; dev-stack e2e clones from the
harness git server (below) and the completion event arrives over SSE.

### 7.5 — e2e harness: real git, fake GitHub (Track A; with 7.3/7.4)
Extend the dev harness with a **local git smart-HTTP server** (real `git http-backend`
or equivalent) requiring bearer-style basic auth, fronted by the existing fake-GitHub
token endpoints; point `PROTEOS_GIT_HOST` at it. Full flow under test: login → machine →
channel → `repos` (fake API) → `clone` (real git, credential helper supplying the fake
token) → commit (identity from `git.configure`) → **push** → assert the push landed and
the token was never written inside the guest; expire the token mid-test → next push
refreshes transparently; revoke → push fails with the clean error.
**Done when:** this runs in normal CI (no KVM, no real GitHub) and is the executable
form of five of the six acceptance criteria.

### 7.6 — React panel + live acceptance (Track A UI; Track B pass; after 7.4)
`ReposPanel` + `GitHubStatus` per decision #8. Then on the Proxmox stack with the real
GitHub App: list repos (verify grant-management link round-trip), clone a real private
repo, commit, push; revoke the grant on github.com → clean failure + reconnect banner →
re-login → push works again; stop/start the machine mid-session → helper still works
(channel re-established). Walk the master-plan Phase 7 checklist and tick the boxes in
`plans/proteos-poc-to-prod.md`; update Phase 5's decision-#8 note per decision #2.

### Sequencing

```
7.0 ──┬──────────────► (facts feed 7.2/7.3/7.4 contracts)
7.1 ──┼──► 7.3 ──┬──► 7.5 ──► 7.6
7.2 ──┴──► 7.4 ──┘
Buildable immediately in parallel: 7.0, 7.1, 7.2. Only 7.6's acceptance pass needs Proxmox.
```

## Acceptance-criteria mapping (master-plan Phase 7 checklist)

| Criterion | Task |
|---|---|
| `GET /api/git/repos` lists the user's GitHub repos | 7.0 (endpoint facts) + 7.4, live in 7.6 |
| `POST /api/git/clone` clones into the workspace via the OpenBao-backed token; no plaintext token on disk | 7.4 + 7.3 (helper-not-URL, decision #7), grep-proof in 7.5 |
| git commit uses the user's GitHub identity | decision #6 (`git.configure`), 7.5 (asserted), 7.6 (live) |
| git push to an authorized repo succeeds from the machine | 7.5 (real git push in CI), 7.6 (real GitHub) |
| Token refresh/expiry handled; revoked grant cleanly fails | 7.2 (TokenSource) + 7.5 (mid-test expiry/revocation) |
| Tokens short-lived, fetched on demand by the helper, never written to disk in the VM | decisions #1/#5, 7.3 + 7.5 (filesystem scan) |

## Critical existing files to modify

- `controlplane/internal/github/client.go` — refresh + repos API (Exchange exists; nothing else does)
- `controlplane/internal/auth/auth.go` — ensure email/name capture is sufficient for
  decision #6 (7.0 verifies); re-login clears `revoked`
- `controlplane/internal/machine/` broker — guestctl subscribes to state transitions
- `controlplane/internal/httpapi/server.go` — git routes; guestctl wiring
- `controlplane/internal/config/config.go` — `GITHUB_APP_SLUG`, `PROTEOS_GIT_HOST`
- `guestagent/cmd/guestagent/main.go` — subcommand dispatch (serve vs git-credential)
- `guestagent/internal/server/` — register `/control` alongside `/terminal`
- `web/src/Dashboard.tsx`, `web/src/api/client.ts`
- `plans/phase-5-implementation.md` — decision-#8 note superseded (identity → "when a
  guest-initiated transport exists"), per decision #2
- `.github/workflows/ci.yml` — 7.5 harness job (plain runner)

## Verification

- **Unit/integration (any OS):** TokenSource matrix (refresh/rotation/revocation/
  concurrency); channel manager lifecycle (up on running, down on stop, reconnect);
  authz choke point (unknown op, foreign host, non-https); credential protocol parsing;
  gitconfig idempotency; log/filesystem redaction scans.
- **e2e (Mac, normal CI):** 7.5 — the full clone/commit/push/refresh/revoke story with
  real git against the harness server, token never on disk.
- **Live (Proxmox):** 7.6 — real App, real private repo, real revocation drill,
  stop/start mid-session.
- **CI:** no new KVM-gated work; 7.5 job on standard runners.

## Non-goals / deferred

- **Per-machine identity secret** — still unminted (decision #2); revisit when a
  guest-initiated transport appears (Phase 11 cross-host routing is the likely trigger).
- **Unifying Phase 5's secret push onto the control channel** — later cleanup; two
  mechanisms is acceptable, churning Phase 5's plan mid-flight is not.
- **SSH-protocol git, multiple git hosts (GitLab etc.)** — the helper/channel design
  doesn't preclude them; GitHub-App-over-HTTPS is the Phase 7 contract.
- **Repo browser, branch pickers, PR creation, commit UI** — Phase 8 (code-server has
  git UI) and Phase 9 (desktop); this phase ends at clone/commit/push from the terminal.
- **Workspace durability for clones** — Phase 4 (explicitly: until it lands, a stopped
  machine loses its clones; demoable behavior is unaffected).
- **Fine-grained per-op audit beyond repos/clone/credential rows** — Phase 10 extends
  the same table.
- **Guest-activity ("not idle") reporting over the control channel** — Phase 11; the
  channel is built to carry it, deliberately not implemented now.
