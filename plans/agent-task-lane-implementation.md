# Plan: Agent Task lane — headless coding-agent tasks via API

> Source: in-conversation design (2026-06-20) — the "mobile agent delegates a task
> to ProteOS" big-picture. A caller (mobile app's own agent, or the browser) hands
> ProteOS a natural-language task; ProteOS runs a coding agent **headlessly** inside a
> machine and streams structured progress back, leaving a reviewable result.
> Off the master-plan 1–12 numbering (feature work); stages labelled **AT1–AT4** to
> avoid colliding with master-plan phase numbers. Layers on top of the **Phase 7**
> control channel (`guestctl`), **Phase 5/6** secret injection + provider registry,
> and the machine SSE stream.
>
> **Companion plan:** `git-review-control-implementation.md` (GR1–GR5). The two meet
> at one seam — see "Relationship to the git-review plan" below. This plan produces a
> dirty working tree; that plan reviews and ships it. They are independently
> shippable and join at `POST .../git/pr`.

## Scope

Let a caller run a coding agent **non-interactively** against a repo cloned in a
machine's `/workspace`, and observe it as a first-class **Task** resource rather than
by screen-scraping a terminal:

1. **Run** — `POST .../tasks {prompt, provider, project}` dispatches a headless agent
   run and returns a `task_id` immediately.
2. **Observe** — poll task status/result, or subscribe to a live stream of **structured
   agent events** (assistant text, tool calls, tool results, final result + usage).
3. **Cancel** — stop a running task.
4. **Continue** — send a follow-up turn ("now also fix the tests") that resumes the
   same agent session.

The agent run **produces a dirty working tree and stops there.** Commit / push / PR are
*not* part of this lane — they are the separately-authorized git-review endpoints
(GR plan). This is what keeps the "human must review before commit" policy intact: the
task lane has no path that commits or pushes.

## Context

The interactive path already exists: `/gw/agent/{provider}` upgrades a WebSocket and
attaches the provider CLI to a **PTY** (`guestagent/internal/term/session.go`), driven
by keystrokes, with scrollback replay. That path is human-shaped — driving it
machine-to-machine means parsing ANSI and guessing when the agent is "done." This plan
adds a **second, headless lane** instead, and reuses the machinery that already exists:

- **Transport** — the Phase 7 control channel (`guestctl` ↔ `ctlchan`,
  `guestwire.ControlFrame`) already carries async, correlated RPC with completion
  notifications (the `git.clone` → `git.clone.done` pattern is the exact template for a
  long-running, fire-and-forget op that streams a result back later).
- **Secrets** — `injector.Inject()` already pushes the user's provider key into the
  guest as a sourceable env file (`/run/proteos/env/<key>.env`), exactly as the
  interactive agent route does before upgrade. The headless run inherits the same env;
  `ANTHROPIC_API_KEY` is simply present.
- **Provider registry** — `providers` table + `PROVIDERS.md` already define each
  provider's launch command, enabled flag, and secret fields. We reuse it to gate which
  providers the headless lane accepts.
- **Eventing** — the machine SSE stream + `machine_events` table are the template for
  surfacing task progress and completion.
- **Audit** — `ActionAgentLaunch` already exists for the interactive route; the headless
  run gets its analogue.

What is genuinely new: a **tasks table** (CP-persisted task lifecycle), a headless
**runner in the guest** that spawns `claude -p --output-format stream-json` and relays
its event stream over the control channel, and the **REST + SSE surface** for tasks.

## Architectural decisions

Durable decisions that apply across all stages:

- **Routes — machine-scoped, nested under the machines resource:**
  - `POST /api/machines/{id}/tasks`              `{prompt, provider, project, branch?}` → `202 {task_id}`
  - `GET  /api/machines/{id}/tasks`              → list this machine's tasks
  - `GET  /api/machines/{id}/tasks/{tid}`        → status + result (usage/cost, session id, summary, changed-files pointer)
  - `GET  /api/machines/{id}/tasks/{tid}/events` → **SSE** stream of structured agent events
  - `POST /api/machines/{id}/tasks/{tid}/cancel` → request cancellation
  - `POST /api/machines/{id}/tasks/{tid}/messages` `{prompt}` → follow-up turn (resume session)
- **Tasks are CP-persisted.** A new `tasks` table holds `{id, machine_id, user_id,
  provider, project, prompt, status, agent_session_id, usage_json, result_summary,
  error, created_at, started_at, ended_at}`. Unlike `projects.list` (filesystem is
  source of truth), a *task* is orchestration state the guest filesystem can't hold, so
  the CP owns it. `agent_session_id` is the coding agent's own session id, captured from
  the run, used to resume for multi-turn.
- **Status state machine:** `queued → running → done | failed | canceled`. Terminal
  states are immutable. No `committed`/`pushed`/`pr_open` states — those belong to the
  git-review lane; a task ends when the agent run ends, leaving a dirty tree.
- **Headless runner = `claude -p` with structured streaming.** The guest spawns the
  provider's launch command in print/non-interactive mode with a machine-readable event
  stream — for Claude Code: `claude -p "<prompt>" --output-format stream-json --verbose`,
  with permissions bypassed for unattended execution (`--dangerously-skip-permissions`
  or the equivalent `--permission-mode`). Each JSON event line is relayed over the
  control channel; the final `result` event yields status + usage/cost + session id.
  **Exact flags must be pinned against the installed CLI version in AT1** — treat the
  above as the shape, not a frozen contract.
- **Unattended permission bypass is safe *because* the microVM is the sandbox.** The
  permission system exists to gate an agent's filesystem/network reach; here the agent
  is already confined to a disposable, isolated, per-user Firecracker machine with no
  ambient credentials beyond the injected provider key and the on-demand git token. The
  VM boundary *is* the approval boundary.
- **Provider limitation — headless lane is Claude Code only (initially).** Claude Code
  has the mature non-interactive story (`stream-json`, structured tool events, resumable
  sessions, permission modes). Gemini/Codex stay available on the **interactive** lane;
  the registry-driven design means adding them to the headless lane later is config +
  one runner branch, not a rearchitecture. `POST .../tasks` rejects a non-headless
  provider with `400 provider_not_headless`; absence of the user's provider key →
  `409 no_provider_key` (same check as `/gw/agent`).
- **Transport pattern — async like `git.clone`.** A new CP→Guest verb `agent.run`
  acks immediately; the guest streams `agent.event` frames (Guest→CP) as the run
  produces them and a terminal `agent.done` frame with the final outcome. `agent.cancel`
  (CP→Guest) signals the running process. The CP persists events → updates task row →
  fans out to the task SSE stream.
- **Project & branch.** `project` targets a repo under `/workspace`, validated exactly
  like `?cwd=` / the GR endpoints. Optional `branch` checks out/creates a feature branch
  *before* the run by delegating to the GR `git.branch` verb — a convenience so the
  agent works off `main`; if GR2 isn't built yet, `branch` is rejected as unsupported.
- **Auth & safety.** All endpoints require an authenticated session; mutating ones
  (`POST tasks`, `cancel`, `messages`) require the `X-Requested-By: proteos` CSRF header
  and machine owned-by-caller + running + live channel. Task creation is audited
  (`ActionAgentTaskRun`); the prompt is stored but agent event payloads are streamed and
  retained per a bounded policy (see AT2). Provider tokens never appear in task records
  or event logs — the injector/credential-helper paths remain the only token carriers.

## Relationship to the git-review plan

```
              ── AT (this plan) ──────────►  ── GR (companion plan) ──────────►
 caller  POST /tasks   agent runs headless    GET /git/status + /git/diff   POST /git/commit
   │     (claude -p)   → DIRTY WORKING TREE    + port-preview test            /git/push  /git/pr
   ▼          │                │                      │                            │
 task_id ◄────┘     SSE: agent events          human (or caller) reviews     PR URL ◄─┘
```

- **The seam is the dirty working tree.** AT ends; GR begins. There is no autonomous
  commit inside ProteOS.
- **Autonomous "task → PR" is caller-orchestrated.** A caller that wants the full loop
  chains AT (`POST /tasks` → wait for `done`) then GR (`/git/commit` → `/git/push` →
  `/git/pr`). For **policy customers**, a human-approval step is inserted between the two
  lanes — which is trivial precisely because they are separate API calls, not one
  opaque autonomous action.
- A task **result does not contain `pr_url`/`commit`** — those come from the GR calls
  the caller makes afterward and are correlated client-side (or via audit).

---

## Phase AT1: Headless run → persisted result

**User stories**: "Hand ProteOS a task in plain language and have a coding agent do the
work headlessly, then tell me it's done and what it did — without me driving a
terminal."

### What to build

The core tracer bullet, end-to-end. The `tasks` table; `POST /api/machines/{id}/tasks`
that validates provider (headless-capable + key present) and project, injects secrets,
dispatches the new async `agent.run` control-channel verb, and returns `202 {task_id}`
with status `queued`. The guest runner spawns `claude -p "<prompt>" --output-format
stream-json --verbose` (permissions bypassed) in the project cwd, consumes the JSON
event stream, and on completion sends `agent.done` with the final status, usage/cost,
agent session id, and result summary. The CP persists this and flips the task to
`done`/`failed`. `GET /api/machines/{id}/tasks/{tid}` returns status + result. Live
event streaming is AT2; here, completion is observable by polling. Demoable: POST a
task against a cloned repo, poll until `done`, then see the changes via GR `git/status`
(or the terminal).

### Acceptance criteria

- [ ] `tasks` table + migration with the documented columns and status enum.
- [ ] `agent.run` (CP→Guest) and `agent.done` (Guest→CP) verbs added to the wire protocol; guest runner spawns the headless CLI in the project cwd with injected provider env and bypassed permissions; **flags pinned against the installed CLI version**.
- [ ] `POST /api/machines/{id}/tasks` returns `202 {task_id}`; rejects non-headless provider (`400 provider_not_headless`), missing key (`409 no_provider_key`), bad project (`400 bad_project`); enforces CSRF + ownership + running + live channel; audited as `ActionAgentTaskRun`.
- [ ] `GET /api/machines/{id}/tasks/{tid}` returns status and, when terminal, the result (usage/cost, agent session id, summary); `GET .../tasks` lists the machine's tasks.
- [ ] A real run against a cloned repo reaches `done`, captures the agent session id, and leaves a dirty working tree visible via GR `git/status` / `projects.list`.
- [ ] A run whose agent errors reaches `failed` with a sanitized (token-free) error; provider-key absence is caught pre-dispatch.
- [ ] Tests cover dispatch validation, a mocked successful run (fake stream-json producer), and a failed run.

---

## Phase AT2: Live structured event stream

**User stories**: "Watch the agent work in real time — what it's thinking, which tools
it's calling, what it changed — as structured events I can render on mobile, not raw
terminal bytes."

### What to build

Real-time observability. The guest relays each stream-json line as an `agent.event`
frame (Guest→CP) as it arrives; the CP persists a bounded event history per task and
fans out to `GET /api/machines/{id}/tasks/{tid}/events` (SSE), with `Last-Event-ID`
replay like the machine SSE stream so a reconnecting client (mobile coming back to
foreground) resumes cleanly. Events are normalized to a small typed schema
(`assistant_text`, `tool_use`, `tool_result`, `result`) so callers don't parse raw CLI
output. The browser desktop gets a live **Task** view; the mobile agent consumes the
same SSE.

### Acceptance criteria

- [ ] `agent.event` (Guest→CP) verb streams normalized events as the run produces them; CP persists a bounded per-task event history (documented cap, with truncation logged).
- [ ] `GET /api/machines/{id}/tasks/{tid}/events` is an SSE stream emitting the typed event schema, with snapshot-on-connect + `Last-Event-ID` replay; closes on terminal task state.
- [ ] Event payloads are normalized (not raw CLI JSON) and carry no tokens/secrets.
- [ ] Browser Task view renders the live stream; the same endpoint serves the mobile/agent caller.
- [ ] Tests cover live fan-out, reconnect/replay, the bounded-history cap, and clean close on completion.

---

## Phase AT3: Cancel a running task

**User stories**: "Stop a task that's going the wrong way (or that I no longer want)
without destroying the whole machine."

### What to build

Cancellation. `POST /api/machines/{id}/tasks/{tid}/cancel` triggers a new `agent.cancel`
(CP→Guest) verb that signals/terminates the running agent process for that task; the
guest reports the run ended via `agent.done` with status `canceled`; the CP transitions
the task to `canceled` (no-op if already terminal). The working tree is left as-is
(whatever partial changes exist) for the user to review or discard via the GR/terminal
surface.

### Acceptance criteria

- [ ] `agent.cancel` (CP→Guest) verb that terminates the named run's process group in the guest.
- [ ] `POST .../tasks/{tid}/cancel` transitions a `running` task toward `canceled`; idempotent / no-op on already-terminal tasks; CSRF + ownership enforced; audited.
- [ ] The event stream and `GET .../tasks/{tid}` reflect `canceled`; partial changes remain in the working tree.
- [ ] Tests cover cancel-while-running, cancel-after-done (no-op), and the resulting status/SSE transitions.

---

## Phase AT4: Multi-turn follow-up

**User stories**: "After reviewing the result, send a follow-up — 'now also update the
tests' — and have the agent continue from where it left off, not start cold."

### What to build

Conversational continuation. `POST /api/machines/{id}/tasks/{tid}/messages {prompt}`
starts a new run that **resumes the stored agent session** (`claude --resume
<agent_session_id> -p "<prompt>"`), reusing the same secret injection and project cwd,
and appends its events to the same task's stream (or a linked follow-up task — decided
in this phase). Status returns to `running` for the new turn and back to a terminal
state when it completes; the agent session id is refreshed if the CLI rotates it.

### Acceptance criteria

- [ ] Guest runner supports a resume mode keyed on the stored agent session id; `POST .../tasks/{tid}/messages` is rejected if no session id was captured (`409 no_session`).
- [ ] A follow-up turn continues the prior agent context (verified by a context-dependent prompt) and streams its events through the existing task SSE.
- [ ] Task status cycles `done → running → done` across turns; the stored session id stays current.
- [ ] CSRF + ownership enforced; each turn audited; the follow-up prompt stored.
- [ ] Tests cover a successful resumed turn, the no-session rejection, and the status cycling.

---

## Non-goals (this plan)

- **Commit / push / PR.** The agent stops at a dirty tree; shipping is the GR plan.
  Autonomous task→PR is caller-orchestrated by chaining the two lanes.
- **Multiple headless providers (Gemini/Codex) on day one.** Claude Code only initially;
  others stay on the interactive lane and are a later config-driven addition.
- **A queue / scheduler across machines.** A task runs in the machine it's posted to;
  cross-machine scheduling, fan-out, and retry policy are out of scope.
- **Cost budgets / quotas / rate-limiting per user.** Usage/cost is captured and surfaced
  but enforcing limits is a separate concern.
- **Replacing the interactive `/gw/agent` lane.** Humans keep the PTY-attached
  multi-provider terminal; this is an additional headless lane, not a replacement.
- **Long-term event retention / analytics.** Event history is bounded per task for live
  observability, not an audit-grade archive.
