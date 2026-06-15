# Phase 6 Implementation Plan: Provider registry + remaining agents (Gemini, OpenAI Codex, pi.dev)

> Source: `plans/proteos-poc-to-prod.md` Phase 6, planned 2026-06-11.
> Status: **Track A landed** (6.1 registry generalization + seeds, 6.2 guest
> setup_command + degraded state, 6.4 data-driven UI, 6.5 fifth-provider e2e).
> Track B pending a Linux/Proxmox host: 6.3 (image bake — `build-rootfs.sh` +
> `PROVIDERS.md` written, pins `TODO(bake)`) and 6.6 (live acceptance).
>
> **Migration numbering:** Phase 5's `000004` had already shipped (5.7 closed),
> so per the header note this phase added a NEW migration `000005` rather than
> folding the schema change into `000004`. Phase 5's decision #4/#5 are therefore
> unchanged — `000004` keeps its `secret_env` shape in history; `000005` migrates
> it forward to `secret_fields` + `setup_command`.
>
> **Decision #7 design note (closes "verified by design/review"):** the
> 5th-provider criterion is an executable test, `TestFifthProviderE2E`
> (`controlplane/internal/httpapi/fifth_provider_e2e_test.go`). It onboards
> arbitrary `stub`/`plain`/`gate` providers entirely as data (a `providers` row
> inserted via SQL at runtime + a launch script in the dev guest), sets their keys
> through the public API, and launches them through `/gw/agent/<key>` — with zero
> provider-specific Go/TS. It also covers the matrix: two providers keyed on one
> machine both launchable; a failing `setup_command` degrades a provider and its
> launch closes 4003 `setup_failed`; key rotation re-runs setup and clears the
> degraded state. The web no-literals half is enforced by a grep
> (no provider key literals in `web/src`).
> Prerequisites: Phase 5 (`plans/phase-5-implementation.md`) — **planned, not landed.** Phase 6
> is a thin layer over Phase 5's contracts: the `providers` table, the generic
> `WS /gw/agent/{provider}` route, the injector push (`PUT /secrets` to the guest), and the
> rootfs bake pattern. If Phase 6 starts before Phase 5's migration `000004` has shipped to
> any real deployment, fold the schema generalization below into `000004` instead of adding
> `000005`. Phase 4 status (persistent `~/.config` state for the CLIs) is inherited via
> Phase 5's note — nothing here adds a new dependency on it.

## Context

Phase 5 builds the provider mechanism with one row (Claude Code) and deliberately generic
plumbing: the registry decides commands and env, the control plane pushes secrets, the
guest spawns `agent-<key>` sessions, the browser only ever names a provider key. Phase 6
proves the "data, not code" claim by adding **three providers without touching the
control-plane proxy/injector/session code**: Gemini CLI, OpenAI Codex, and pi.dev. The
master-plan acceptance is explicit: a hypothetical 5th provider must require only a
registry entry + template — this plan makes that an **executable test**, not a design
review.

Reality of the three new CLIs (verified 2026-06-11; task 6.0 re-verifies and pins):

1. **Gemini CLI** is npm-distributed (`@google/gemini-cli`) and honors `GEMINI_API_KEY` —
   the rootfs therefore needs a **pinned Node runtime** as a base capability.
2. **Codex CLI** is a static musl binary (no Node), but API-key auth is a login step
   (`printenv OPENAI_API_KEY | codex login --with-api-key`, writing `~/.codex/auth.json`)
   rather than pure-env. This forces the one genuinely new mechanism of the phase: an
   optional, registry-declared **`setup_command`** run by the guest agent — data-driven,
   so the next provider with login-style auth is still just a row.
3. **pi.dev has no API key of its own** — it is a multi-provider agent that reads
   *model-provider* keys from env (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`,
   …) or `~/.pi/agent/auth.json`. This stresses the registry shape: secret fields must be
   **declared per provider** (name + label + env var), not assumed to be a single
   `api_key`.

These three facts produce the three design moves below; everything else is seeds, image
work, and UI.

## Architecture decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | **Registry schema generalized** (migration `000005`, or folded into `000004` per the header note): `providers` drops Phase 5's `secret_env` in favor of `secret_fields jsonb` — an ordered list of `{"name":"api_key","label":"Anthropic API key","env":"ANTHROPIC_API_KEY"}` — and gains `setup_command text NULL`. Seed rows: `claude` (unchanged behavior, restated in the new shape), `gemini` (`GEMINI_API_KEY`), `openai` (`OPENAI_API_KEY` + `setup_command: printenv OPENAI_API_KEY \| codex login --with-api-key`), `pi` (field `anthropic_api_key`, label "Anthropic API key (used by Pi)", env `ANTHROPIC_API_KEY`). | One shape covers all four observed auth styles (pure env, login-step, borrowed-model-key) and renders the settings UI from data (decision #5). Field *names/labels/env vars* are not secret — only values are. |
| 2 | **Per-provider secret isolation is kept, even when keys duplicate**: pi's Anthropic key is stored under `secret/users/<uid>/providers/pi`, not read from claude's path. | A registry row that could reference *another provider's* secret path is a cross-provider read primitive — exactly the kind of reach-through Phase 10 would have to unwind. Duplicate paste of one key is a minor UX cost; strict path-per-provider keeps the OpenBao policy story (Phase 5 decision #2) and the injector trivially per-provider. |
| 3 | **`setup_command` semantics**: delivered to the guest in the same `PUT /secrets` payload; the guest agent runs it as a root login shell command **on every push** (start, resume, key rotation), after env files are written, asynchronously, output to the guest-agent log. Setup commands must therefore be **idempotent** — documented in the registry seed and `image/PROVIDERS.md`. Failures mark the provider definition degraded; launching it then surfaces a clear error frame instead of a broken TUI. | Run-on-every-push makes key rotation re-login automatically and adds no "has it run yet" state machine. Codex writing `~/.codex/auth.json` (plaintext, inside the VM) is acceptable: it is the user's own key in their own VM, on the Phase 4 encrypted disk — same posture as `~/.claude` state. |
| 4 | **Rootfs gains a pinned Node LTS runtime + the three CLIs** via `image/build-rootfs.sh`: Node (tarball install, pinned), `@google/gemini-cli` and the pi.dev coding agent (npm, pinned versions, installed globally at bake time), Codex (pinned musl binary). Versions + sha256 recorded in `manifest.lock`; new `image/PROVIDERS.md` records each CLI's install method, auth mechanism, first-run behavior, and re-verification steps (written by task 6.0). | Same version-pinned-image philosophy as Claude Code in Phase 5. Node is shared infrastructure for current and future npm-distributed agents. Install-at-first-launch inside VMs stays rejected (slow, flaky, unpinned). |
| 5 | **Settings UI is rendered from registry metadata** — `GET /api/providers` now returns `secret_fields` (names, labels, env names) and `key_set`; `ProvidersPanel` renders one form per provider from that data; `PUT /api/secrets/providers/{key}` takes `{"fields":{"<name>":"<value>"}}` validated against the declared field names (unknown field → 422, missing required → 422). Launch buttons appear for every enabled provider with `key_set`, all opening `/gw/agent/<key>`. | Zero per-provider React code is the front-end half of the "5th provider = data only" criterion. The fields-map body is the general shape Phase 5's single-`api_key` body grows into (or starts as, per the header note). |
| 6 | **First-run TUI wizards are allowed; only auth is pre-wired.** These CLIs run in a real PTY session the user is watching — theme pickers, approval-mode prompts, and onboarding screens are interactive and fine. The contract is narrower than "headless": *authentication* must come from injected env (or `setup_command`), never from a browser-visible secret prompt. Approval/sandbox mode flags stay at CLI defaults this phase; tuning them is a `launch_command` data change later. | Keeps scope honest: chasing fully-promptless first runs for four TUIs is busywork the PTY model doesn't need. Auth-from-env is what the acceptance criterion ("authenticates with the injected key") actually requires. |
| 7 | **The 5th-provider criterion becomes an executable test**: the dev-stack e2e inserts a `stub` provider row at runtime (launch command = a script in the dev guest that prints its env), sets its key through the public API, and launches `/gw/agent/stub` — asserting the full chain works **with zero Go/TS changes**. A short design-review note in this plan's PR closes the "verified by design/review" wording. | A test that fails if someone hardcodes a provider key anywhere is stronger than a review, and it documents the onboarding recipe by example. |

## Wire contracts

### Registry row shape (seeds; migration `000005` or folded into `000004`)

```
key      display_name   launch_command  setup_command                                  secret_fields
claude   Claude Code    claude          —                                              [{name:api_key, label:"Anthropic API key", env:ANTHROPIC_API_KEY}]
gemini   Gemini CLI     gemini          —                                              [{name:api_key, label:"Gemini API key", env:GEMINI_API_KEY}]
openai   OpenAI Codex   codex           printenv OPENAI_API_KEY | codex login --with-api-key
                                                                                       [{name:api_key, label:"OpenAI API key", env:OPENAI_API_KEY}]
pi       Pi             pi              —                                              [{name:anthropic_api_key, label:"Anthropic API key (used by Pi)", env:ANTHROPIC_API_KEY}]
```

### API (generalized from Phase 5)

```
GET  /api/providers                  → 200 [{"key":"gemini","display_name":"Gemini CLI","enabled":true,
                                             "key_set":false,
                                             "secret_fields":[{"name":"api_key","label":"Gemini API key",
                                                               "env":"GEMINI_API_KEY"}]}, …]
PUT  /api/secrets/providers/{key}    body {"fields":{"api_key":"…"}}
                                     → 204 · 404 unknown provider · 422 unknown/missing field
WS   /gw/agent/{provider}            → unchanged (any enabled registry key; no per-provider code)
```

### Guest push payload (guestwire, extended)

```
PUT /secrets {"providers":{
  "openai":{"command":"codex",
            "setup_command":"printenv OPENAI_API_KEY | codex login --with-api-key",
            "env":{"OPENAI_API_KEY":"sk-…"}}, …}}    → 204
semantics: replace-all (Phase 5); setup_command runs per push, idempotent, async,
           logged; setup failure ⇒ provider marked degraded ⇒ launch closes 4003
           provider_unavailable with reason "setup_failed"
```

## Package layout (new / touched — note how little is new code)

```
controlplane/
  migrations/000005_provider_fields.*.sql   # secret_fields + setup_command + 3 seed rows (or fold → 000004)
  internal/providers/                       # fields metadata, validation by declared fields
  internal/httpapi/providers.go             # fields-map body; metadata in GET
  internal/injector/injector.go             # carry setup_command; env from secret_fields
guestagent/
  internal/secrets/secrets.go               # setup_command runner (idempotent, logged, degraded flag)
  api/                                      # payload field; close-reason detail
image/
  build-rootfs.sh                           # + Node LTS, gemini-cli, pi, codex (all pinned)
  PROVIDERS.md                              # NEW: per-CLI install/auth/first-run findings (task 6.0)
  manifest.lock                             # new pins
web/src/
  components/ProvidersPanel.tsx             # form rendered from secret_fields; launch per provider
  api/client.ts                             # types
e2e: fifth-provider stub test (decision #7) alongside the Phase 5 harness
```

## Tasks (Track A = Mac/dev-driver; Track B = Proxmox/Firecracker)

### 6.0 — Provider onboarding checklist → `image/PROVIDERS.md` (Track A; standalone, do first)
For each of gemini / openai / pi, in a container matching the rootfs base (Ubuntu 24.04
x86_64): pin the exact artifact (npm version / binary release + sha256), prove
**auth-from-env works** (or determine the exact `setup_command`), record first-run
behavior (wizards, files written under `$HOME`), and the launch command. Re-verify
claude's entry from Phase 5 into the same doc. This file is the input to the 6.1 seeds
and the 6.3 image build — no version numbers enter the migration or build script except
from here.
**Done when:** `PROVIDERS.md` has all four rows complete, each demonstrated in the
container with a real or dummy key (auth failure surfaced *inside* the CLI is fine for
dummy keys — the env path is what's being proven).

### 6.1 — Registry generalization + seeds (Track A; after 6.0)
Migration per decision #1 (+ sqlc regen), `internal/providers` field validation,
httpapi fields-map body + metadata response, injector carries `setup_command` and builds
env from `secret_fields`. Redaction discipline unchanged (values never in logs/responses;
field *names* are fine).
**Done when:** route-table tests cover the new 422s; injector unit test shows a
multi-field, setup-command provider composed correctly; claude round-trips unchanged.

### 6.2 — Guest agent: setup_command + degraded state (Track A; parallel with 6.1)
Runner per decision #3 (root login shell, async, output to log, per-push), degraded flag
per provider, launch of a degraded provider closes 4003 with `setup_failed`; re-push
clears degraded on success.
**Done when:** unit tests prove: setup runs after env files exist; failure → degraded →
4003; success after rotation re-run → launchable; providers without setup_command are
untouched.

### 6.3 — Rootfs: Node + three CLIs (Track B; after 6.0)
`build-rootfs.sh` additions per decision #4, `manifest.lock` pins, image rebuild,
re-pin `PROTEOS_ROOTFS_REF`, stage on the Proxmox host.
**Done when:** a VM from the new image has `claude --version`, `gemini --version`,
`codex --version`, `pi --version` all working as root, and image size + build time are
recorded in `PROVIDERS.md` (this image grows materially — know by how much).

### 6.4 — Data-driven settings + launch UI (Track A; after 6.1)
`ProvidersPanel` rendered from `secret_fields` (write-only inputs per field, `key_set`
badge, set/replace/delete), launch buttons for all enabled+keyed providers opening
`/gw/agent/<key>`; one agent panel at a time (windowing is Phase 9).
**Done when:** the panel renders all four providers with zero per-provider component
code (grep-able: no provider key literals in `web/src` outside tests), launch works
against dev-stack stubs, missing-key CTA per provider.

### 6.5 — Fifth-provider proof + dev-stack e2e (Track A; after 6.1 + 6.2)
Decision #7: e2e inserts a `stub` provider row (multi-field, with a setup_command that
drops a marker file), sets fields via the public API, launches `/gw/agent/stub`, asserts
the session sees its env and the marker exists. Extend the Phase 5 harness matrix:
two providers keyed on one machine → both launchable; one degraded → 4003; key rotation
re-runs setup.
**Done when:** the stub test is green in normal CI and demonstrably involves no
provider-specific Go/TS (it is the documentation of "adding a provider").

### 6.6 — Live acceptance pass (Track B; after 6.3 + 6.4 + 6.5)
On the Proxmox stack with real keys: set keys for all four providers, launch each from
the UI, prompt each to touch a file in the workspace; verify codex's setup login ran
(auth.json present, no key in any log); stop/start → all four re-injected and
re-launchable; reload mid-session reattaches. Walk the master-plan Phase 6 checklist and
tick the boxes in `plans/proteos-poc-to-prod.md`.

### Sequencing

```
6.0 ──┬──► 6.1 ──┬──► 6.4 ──┐
      │          ├──► 6.5 ──┼──► 6.6 (Track B)
      │    6.2 ──┘          │
      └──► 6.3 (Track B) ───┘
Buildable immediately in parallel: 6.0, 6.2. Everything except 6.3/6.6 runs on a Mac.
```

## Acceptance-criteria mapping (master-plan Phase 6 checklist)

| Criterion | Task |
|---|---|
| Registry drives the available-agent list (DB-backed, no hardcoding) | 6.1 + 6.4 (data-driven UI), enforced by 6.5's no-literals grep |
| All four providers registered with required secret keys + launch commands | 6.0 (facts) + 6.1 (seeds) |
| Each provider's key stored/injected via the Phase 5 OpenBao path | 6.1 (same `secret/users/<uid>/providers/<key>` paths; decision #2) |
| User can launch each of the four agents from the UI | 6.3 + 6.4, demoed in 6.6 |
| 5th provider = registry entry + template only, no control-plane code change | 6.5 (executable proof) + design note in this plan's PR |

## Critical existing files to modify

- `controlplane/migrations/` — `000005` (or fold into Phase 5's `000004` if unshipped)
- `controlplane/internal/{providers,injector,httpapi}/` — field metadata, validation, payload
- `controlplane/internal/store/queries.sql` + sqlc regen
- `guestagent/internal/secrets/`, `guestagent/api/` — setup runner, degraded state, payload
- `image/build-rootfs.sh`, `image/manifest.lock` — Node + CLIs; ref re-pin
- `web/src/components/ProvidersPanel.tsx`, `web/src/api/client.ts`
- `RUNBOOK.md` — image restage step; note on image size growth
- `plans/phase-5-implementation.md` — if Phase 6 lands schema changes into `000004`,
  annotate Phase 5's decision #4/#5 so the two plans don't contradict

## Verification

- **Unit/integration (any OS):** field validation 422 matrix; injector composition;
  setup runner semantics (idempotent re-run, degraded, rotation); redaction scans
  extended to multi-field values.
- **e2e (Mac, normal CI):** 6.5 — stub fifth provider end-to-end; two-providers-one-
  machine; degraded path; rotation.
- **Live (Proxmox):** 6.6 — all four real CLIs launched from the UI, auth via injected
  env (codex via setup login), re-injection across stop/start, reattach mid-session.
- **CI:** migration + sqlc diff; web typecheck/build; no new KVM-gated tests (the gated
  job re-runs unchanged against the bigger image).

## Non-goals / deferred

- **Interactive subscription logins** (Claude Pro, ChatGPT sign-in, `/login` flows) —
  they may incidentally work in the PTY but are unsupported/untested; API keys are the
  Phase 6 contract. Revisit with the Phase 7+ trust model if wanted.
- **Per-provider sandbox/approval-mode tuning** (`codex --full-auto`, etc.) — a
  `launch_command` data change, product decision deferred.
- **Admin UI/CLI for registry rows** — seeds via migration; operators use SQL until a
  real need appears (Phase 9 settings or later).
- **Multiple concurrent agent windows, per-provider session multiplexing** — Phase 9
  (the `agent-<key>` naming scheme already leaves room).
- **Egress/cost controls on agent API usage** — Phase 10.
- **Auto-detecting which model providers a pi user has keyed and composing its env from
  them** — rejected for now (decision #2); revisit only if duplicate key entry proves a
  real UX problem.
