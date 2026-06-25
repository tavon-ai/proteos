# Plan: `proteos` CLI — drive the Agent Task lane from the command line

> Source: in-conversation request (2026-06-20) — now that the **Agent Task lane**
> (`agent-task-lane-implementation.md`, AT1–AT4) is built and green, give an agent (or a
> human) a Go command-line client that calls the controlplane HTTP API directly instead
> of through the browser desktop. The mobile/agent caller story from the AT plan becomes
> concrete: a binary that runs a headless task, watches its structured event stream,
> cancels it, and sends follow-up turns.
>
> Off the master-plan 1–12 numbering (feature work); stages labelled **AC1–AC5** to avoid
> colliding with master-plan phase numbers, the AT stages, the GR stages, and PP.
>
> **Depends on:** the Agent Task endpoints from `agent-task-lane-implementation.md`
> (`POST/GET /api/machines/{id}/tasks…`, SSE `/events`, `/cancel`, `/messages`) — all
> landed. **Adds one backend capability the CLI requires:** non-browser authentication via
> **Personal Access Tokens (PAT)**, since the controlplane today only accepts the
> `proteos_session` browser cookie.

## Scope

A single statically-linked Go binary — working name **`proteos`** — that authenticates with
a Personal Access Token and exposes the Agent Task lane as subcommands:

1. **Auth** — `proteos auth login` stores a PAT locally; `proteos auth status` / `logout`.
2. **Machines** — `proteos machines ls` / `proteos machines get <id>` (the CLI needs a
   machine id for every task call).
3. **Run** — `proteos task run --machine <id> --provider claude --project <repo> "<prompt>"`
   dispatches a headless task and prints the `task_id`; `--wait` polls to terminal.
4. **Observe** — `proteos task ls`, `proteos task get <tid>`, and `proteos task watch <tid>`
   (live SSE stream of normalized agent events with reconnect).
5. **Cancel** — `proteos task cancel <tid>`.
6. **Follow-up** — `proteos task send <tid> "<prompt>"` (resume the agent session).

The CLI is a **thin, decoupled HTTP client.** It defines its own request/response DTOs
(the controlplane shapes live under `internal/`, so they are not importable) and talks only
to the public `/api` surface. It commits nothing and pushes nothing — exactly like the AT
lane it drives, it stops at a dirty working tree and reports what the agent did.

## Context

What already exists and is reused:

- **The Agent Task HTTP surface** (`controlplane/internal/httpapi/tasks.go`,
  `taskstream.go`) — the exact endpoints, request bodies (`createTaskRequest`,
  `sendMessageRequest`), and response shapes (`taskView`, `taskIDResponse`) the CLI
  serializes against. SSE events are the normalized `AgentEventPayload`
  (`guestagent/api/guestwire.go:361`) with kinds `assistant_text` / `tool_use` /
  `tool_result` / `result`.
- **Session machinery** (`controlplane/internal/session/session.go`) — the PAT design is a
  near-copy: 32 random bytes → base64 RawURL, SHA-256 hashed at rest, constant-time
  compare, `expires_at` column. We reuse this pattern rather than invent a new one.
- **`requireAuth` middleware** (`controlplane/internal/httpapi/middleware.go:54`) — today
  cookie-only; it is the *single* auth entry point, so adding a Bearer-token fallback there
  lights up every existing authenticated route for the CLI at once.
- **Migration + audit conventions** — `migrations/NNNNNN_name.up.sql` applied at startup via
  golang-migrate (`store.Migrate`); `audit.Recorder.Record` with `Action*` constants. The
  next migration number is `000009`.
- **CLI precedent** — `controlplane`, `nodeagent`, `guestagent` are all stdlib-`flag`
  binaries under `cmd/<name>/main.go`. We match that: no cobra, stdlib `flag` with a small
  subcommand dispatcher.

What is genuinely new: a **`personal_access_tokens` table + token CRUD endpoints +
Bearer-auth path** in the controlplane, and a **new Go module + binary** for the CLI.

## Architectural decisions

Durable decisions that apply across all stages:

- **New module, added to the workspace.** A new `./cli` module
  (`github.com/tavon-ai/proteos/cli`), binary at `cli/cmd/proteos`, added to `go.work`. Keeping
  it a separate module (not a package under `controlplane`) keeps the server's dependency
  graph clean and lets the CLI be built/released independently. The CLI does **not** import
  `controlplane/internal/*`; it owns small DTO structs mirroring the JSON.
- **stdlib `flag`, manual subcommand dispatch.** Matches all three existing binaries; zero
  new third-party deps. Shape: `proteos <group> <command> [flags] [args]`
  (e.g. `proteos task run …`). A top-level switch routes to per-command `flag.FlagSet`s.
  *(If the command tree grows past comfort, cobra is the obvious later swap — noted as a
  non-blocking option, not adopted now.)*
- **Auth = Personal Access Token, sent as `Authorization: Bearer <token>`.** The backend
  gains a PAT system; the CLI stores one token and attaches it to every request. Bearer is
  chosen over a custom header so standard tooling (`curl -H`, HTTP libraries) works too.
- **Bearer tokens are CSRF-exempt.** The `X-Requested-By: proteos` header exists to stop
  cookie-based cross-site requests; a Bearer token is not ambient browser credential, so
  token-authenticated mutating requests skip the CSRF check. The CLI still sends a sane
  `User-Agent`. (Decision encoded in middleware: if auth came from a Bearer token, the CSRF
  gate is satisfied.)
- **Token bootstrap is web-first; the CLI is token-consumer only.** A user mints/revokes
  their PAT from the **Settings → Tokens** page in the browser desktop, copies the
  one-time-shown plaintext, and hands it to the CLI — primarily via the `PROTEOS_TOKEN` env
  var, which is how an agent gets it. The CLI never mints a token; it only stores/sends one.
  The settings page can **revoke** an existing token and **create a fresh one** (the
  copy-and-rotate flow), which is the supported way to recover from a leaked/lost token.
- **Token storage on disk.** `~/.config/proteos/credentials.json` (respecting
  `$XDG_CONFIG_HOME`), file mode `0600`, holding `{base_url, token, login}`. Env overrides
  for headless/CI/agent use: `PROTEOS_URL` and `PROTEOS_TOKEN` take precedence over the file
  so an agent can run with no prior `login` — just the env var the user pasted.
- **Base URL is explicit, no magic discovery.** `--url` flag > `PROTEOS_URL` env > stored
  `base_url` > error. The CLI never assumes `localhost`.
- **Output: human by default, `--json` for machines/agents.** Every read command supports
  `--json` to emit the raw API JSON for scripting; the default is a compact human table.
  This is the single most important affordance for the "agent calls the CLI" use case.
- **Exit codes are meaningful.** `0` success; `1` generic/runtime error; `2` usage error;
  `3` auth error (401/403); `4` not found (404); `5` task ended `failed`/`canceled` (so
  `task run --wait` scripts can branch). Errors print the API `{"error","detail"}` envelope.
- **Token never logged.** Like provider tokens in the AT lane, the PAT appears only in the
  `Authorization` header and the `0600` credential file — never in `--json` output, error
  messages, or audit metadata (only a token-id/prefix is auditable).

---

## Phase AC1: Auth foundation + CLI skeleton (tracer bullet)

**User stories**: "As an agent or operator I can mint a token, point the `proteos` CLI at
the server, authenticate without a browser, and list my machines — proving the whole
client→auth→API path end to end."

### What to build

The thin vertical slice through both halves.

**Backend (PAT):**
- Migration `000009_personal_access_tokens.up.sql` / `.down.sql`: table
  `{id uuid pk, user_id uuid fk users on delete cascade, name text, token_hash bytea unique,
  prefix text, created_at, expires_at nullable, last_used_at nullable, revoked_at nullable}`.
- A `token` package (mirroring `session`): generate 32 random bytes → base64 RawURL, store
  SHA-256 hash, return plaintext **once**; `Authenticate(ctx, token) → (User, tokenRow)`
  with revoked/expired checks and a best-effort `last_used_at` bump.
- Endpoints (cookie-authed, so a browser-logged-in user bootstraps the first token):
  - `POST   /api/tokens` `{name, expires_in_days?}` → `201 {id, name, token, prefix, expires_at}` (plaintext token shown once)
  - `GET    /api/tokens` → list `{id, name, prefix, created_at, last_used_at, expires_at}` (no hash, no plaintext)
  - `DELETE /api/tokens/{id}` → `204` (revoke; sets `revoked_at`)
- Extend `requireAuth` (`middleware.go:54`) to accept `Authorization: Bearer <token>` as an
  alternative to the cookie: try Bearer first, fall back to cookie. Mark the request as
  token-authenticated so `csrfHeader` passes for Bearer requests.
- New audit actions `ActionTokenCreate` / `ActionTokenRevoke`; token create/revoke audited
  (metadata carries token id + name + prefix, **never** the secret).

**Web (the bootstrap surface):**
- A **Settings → Tokens** tab in the existing tabbed settings window
  (`web/src/windows/SettingsWindow.tsx` — add `'tokens'` to the `Tab` union alongside
  `'providers'`/`'github'`) with a new `web/src/components/TokensPanel.tsx` following the
  `ProvidersPanel` pattern.
- API methods in `web/src/api/client.ts` and react-query hooks in `web/src/api/hooks.ts`
  (mirroring `useProviderMutations`) hitting the AC1 endpoints: list, create, revoke. They
  ride the existing `request()` wrapper, so the cookie + `X-Requested-By: proteos` header
  come for free.
- UX mirrors the secrets pattern: the plaintext token is shown **once** on create (with a
  copy button and a "store this now, it won't be shown again" note); the list thereafter
  shows only name/prefix/created/last-used/expiry with a **Revoke** action; a
  **Create new token** action supports the copy-and-rotate flow.

**CLI:**
- New `./cli` module + `go.work` entry; `cli/cmd/proteos/main.go` with subcommand dispatch.
- `internal/client`: HTTP client that injects `Authorization: Bearer`, a `User-Agent`,
  decodes the `{error,detail}` envelope into a typed error, and maps status → exit code.
- `internal/config`: load/save `credentials.json` (`0600`), env overrides.
- Commands: `proteos auth login --token <t> [--url <u>]` (verifies by calling `GET /api/me`,
  then persists), `proteos auth status`, `proteos auth logout`, `proteos machines ls`,
  `proteos machines get <id>` — all with `--json`.

### Acceptance criteria

- [ ] Migration `000009` creates `personal_access_tokens`; `store.Migrate` applies it cleanly up and down.
- [ ] `token` package generates/hashes/validates exactly like `session` (hash at rest, constant-time compare, expiry + revocation honoured); unit-tested.
- [ ] `POST /api/tokens` returns the plaintext token once and never again; `GET /api/tokens` never exposes hash or plaintext; `DELETE` revokes; all three are audited.
- [ ] `requireAuth` authenticates a valid `Authorization: Bearer` token (attaching the same user/context as a cookie would) and rejects expired/revoked/garbage with `401`; Bearer requests satisfy the CSRF gate.
- [ ] An **existing** authenticated route (e.g. `GET /api/me`, `GET /api/machines`) works unchanged with either a cookie or a Bearer token — no per-route changes needed.
- [ ] **Settings → Tokens** tab lets a logged-in user create a token (plaintext shown once, with copy + warning), see their tokens (name/prefix/created/last-used/expiry, no secret), revoke one, and create a fresh one — the copy-and-rotate flow; all via the existing CSRF-bearing `request()` wrapper.
- [ ] `proteos auth login --token … --url …` verifies against `GET /api/me`, stores `credentials.json` at `0600`, and `auth status` reports the logged-in login; `PROTEOS_TOKEN`/`PROTEOS_URL` override the file.
- [ ] `proteos machines ls` prints a human table (id, name, state) and `--json` prints the raw array; bad/expired token exits `3`.
- [ ] Backend tests cover token CRUD + Bearer auth + CSRF-exemption; CLI tests cover client error/exit-code mapping and config round-trip against an `httptest` server.

---

## Phase AC2: Run a task and inspect it

**User stories**: "Hand the CLI a plain-language task against a cloned repo and get a
`task_id`; later (or with `--wait`) see its status and result — usage, cost, summary,
session id — without opening a browser."

### What to build

The core task verbs over `tasks.go`.

- `proteos task run --machine <id> --provider claude --project <repo> [--wait] "<prompt>"`
  → `POST /api/machines/{id}/tasks` with `{prompt, provider, project}`, sends the CSRF
  header (harmless under Bearer), prints `task_id`. `--wait` polls `GET …/tasks/{tid}` until
  terminal and exits `0` on `done`, `5` on `failed`/`canceled`. Prompt can also be read from
  stdin (`--prompt-file -`) for long/multiline prompts an agent assembles.
- `proteos task ls --machine <id>` → `GET …/tasks`, table of (id, status, provider, project,
  created); `--json` raw.
- `proteos task get --machine <id> <tid>` → `GET …/tasks/{tid}`, renders `taskView`
  including usage/cost, `agent_session_id`, `result_summary`, `error` when terminal.
- Surface the documented dispatch errors verbatim: `provider_not_headless` (400),
  `no_provider_key` (409), `bad_project`/`bad_request` (400), `machine_not_running` (409),
  `no_machine`/`no_task` (404) — each mapped to a clear message + the right exit code.

### Acceptance criteria

- [ ] `task run` posts the correct body, prints `task_id`, and exits `0` immediately without `--wait`.
- [ ] `--wait` polls to a terminal state with a bounded interval/backoff and a `--timeout`; exits `0` on `done`, `5` on `failed`/`canceled`; prints the final summary/error.
- [ ] Prompt accepted as a positional arg, `--prompt-file <path>`, or stdin (`-`).
- [ ] `task ls` and `task get` render human + `--json`; `get` shows usage/cost/session-id/summary when terminal.
- [ ] Each documented dispatch error is shown clearly and mapped to its exit code (auth=3, not-found=4, terminal-fail=5, else 1).
- [ ] Tests against an `httptest` server cover run, `--wait` polling (queued→running→done and →failed), and the error-code mapping; a real run against a live machine reaches `done` and shows the dirty tree via `git status` / the desktop.

---

## Phase AC3: Live structured event stream

**User stories**: "Watch the agent work in real time from my terminal — assistant text,
tool calls, tool results, and the final result — as structured events, resuming cleanly if
my connection drops."

### What to build

An SSE consumer for `GET /api/machines/{id}/tasks/{tid}/events`.

- `proteos task watch --machine <id> <tid>`: opens the SSE stream, parses `id:`/`event:
  agent`/`data:` frames, and renders each normalized `AgentEventPayload` kind:
  - `assistant_text` → the text;
  - `tool_use` → `▸ <tool>` with bounded input (pretty-printed when `--json` off);
  - `tool_result` → bounded output, flagged on `is_error`;
  - `result` → the terminal frame (status, cost_usd, num_turns, duration_ms, error) →
    close and exit (`0` done, `5` failed/canceled).
- **Reconnect with replay**: track the last `id:` seen and reconnect with
  `Last-Event-ID: <n>` on transient drop (the server replays from its ring), honouring the
  heartbeat (`: ping`) to detect dead connections. Bounded retry with backoff.
- `--json` mode emits one normalized event per line (NDJSON) — the agent-friendly form.
- `proteos task run … --watch` composes AC2+AC3: dispatch, then immediately attach the
  stream (instead of polling).

### Acceptance criteria

- [ ] `task watch` connects, renders all four event kinds with bounded input/output, and exits with the right code on the terminal `result` frame.
- [ ] On a dropped connection the client reconnects with `Last-Event-ID` and does not duplicate or drop events across the gap; heartbeats are tolerated, a truly dead stream is detected.
- [ ] `--json` emits NDJSON of normalized events (no `task_id` leakage beyond what the API sends, no secrets).
- [ ] `task run --watch` dispatches then streams in one invocation.
- [ ] Tests use an `httptest` SSE server to cover normal stream-to-terminal, mid-stream reconnect/replay, and clean close on completion.

---

## Phase AC4: Cancel and multi-turn follow-up

**User stories**: "Stop a task that's going the wrong way, and after reviewing a result send
a follow-up turn — 'now also update the tests' — that continues the same agent session."

### What to build

The remaining two AT verbs.

- `proteos task cancel --machine <id> <tid>` → `POST …/tasks/{tid}/cancel`; idempotent —
  already-terminal tasks report a no-op success, not an error.
- `proteos task send --machine <id> <tid> "<prompt>"` → `POST …/tasks/{tid}/messages`
  `{prompt}`; surfaces `no_session` (409) and `task_running` (409) clearly. Supports
  `--wait` / `--watch` like `task run` so a follow-up can be observed to completion.
- Convenience: `proteos task cancel` accepts `--all-running --machine <id>` to cancel every
  running task on a machine (each call still idempotent), `log`-style reporting of what was
  cancelled (no silent caps).

### Acceptance criteria

- [ ] `task cancel` transitions a running task and is a clean no-op (exit `0`) on an already-terminal task; `--all-running` cancels each and reports the set acted on.
- [ ] `task send` posts the follow-up, surfaces `no_session`/`task_running` with the right exit code, and `--wait`/`--watch` follow the resumed turn to a terminal state.
- [ ] A real follow-up against a prior task continues the agent context and is observable via `task watch`.
- [ ] Tests cover cancel-while-running, cancel-after-terminal (no-op), `send` happy path, and both `send` rejections.

---

## Phase AC5: Packaging, ergonomics, docs (optional polish)

**User stories**: "Install the CLI easily, get helpful `--help` everywhere, and read a doc
that an agent can follow to drive ProteOS."

### What to build

- `proteos version` (embedded build version via `-ldflags`), `proteos --help` and per-command
  help with examples, `proteos completion` for bash/zsh (only if it stays cheap under stdlib
  `flag`; otherwise documented manual completion).
- Taskfile targets (`task cli:build`, `cli:test`, `cli:install`) consistent with the repo's
  `Taskfile.yaml`; a `goreleaser`-style or plain cross-compile build for darwin/linux amd64+arm64.
- `docs/CLI.md`: install, getting a token from **Settings → Tokens**, the env-var contract
  (`PROTEOS_URL`, `PROTEOS_TOKEN`), and a worked end-to-end example (run → watch →
  cancel/send) plus exit codes — written so the mobile/agent caller can consume it.

### Acceptance criteria

- [ ] `proteos version` reports an embedded version; `--help` works at top level and per command with examples.
- [ ] Build/test/install Taskfile targets exist and pass; cross-compiled binaries produced for darwin/linux (amd64+arm64).
- [ ] `docs/CLI.md` documents getting a token from Settings → Tokens, the full task lifecycle, env vars, and exit codes.

---

## Non-goals (this plan)

- **Commit / push / PR from the CLI.** The CLI drives only the AT lane (dirty tree). Wrapping
  the GR git-review endpoints (`/git/commit`, `/git/push`, `/git/pr`) is a separate, later CLI
  surface — the seam is identical to the one between the AT and GR plans.
- **Interactive PTY / terminal attach.** The CLI is headless-task only; the browser keeps the
  `/gw/agent` interactive terminal. No raw terminal proxying from the CLI.
- **Machine lifecycle management** (create/start/stop/delete machines) beyond `ls`/`get`.
  Provisioning stays in the desktop UI for now; the CLI consumes an existing machine.
- **OAuth device flow / browser-based CLI login.** Auth is PAT-only (a deliberate fork chosen
  over device flow); a device flow could be added later without disturbing the token model.
- **Replacing the browser desktop.** This is an additional programmatic surface, not a
  replacement for the web UI.
- **Multiple headless providers in the CLI.** The CLI passes `--provider` through; headless
  support is whatever the AT lane allows (Claude Code initially). No CLI-side provider logic.
- **Cobra / large CLI framework.** Kept to stdlib `flag` to match the repo; revisited only if
  the command tree outgrows it.
