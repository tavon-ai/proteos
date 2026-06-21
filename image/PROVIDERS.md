# Provider CLIs — install, auth, first-run (Phase 6 task 6.0)

This file is the **single source of truth for provider pins**. No version literal
lives in the migration or `build-rootfs.sh`; every version/sha here is what the
bake (`build-rootfs.sh`) and the registry seeds (`migration 000005`) are pinned
to. When a CLI is upgraded, change it here first, then re-bake and re-seed.

Each row records: how the CLI is installed into the rootfs, how it authenticates
(the contract is *auth-from-injected-env*, optionally via a registry
`setup_command` — never a browser-visible secret prompt), what it writes under
`$HOME` on first run, and the launch command the registry stores.

> **Install policy (follows the Claude Code `--claude-bootstrap` pattern):** each
> CLI installs the **latest** version by default; pass a version to pin it. Node,
> Gemini, Codex, and pi.dev are all installed via their package managers, so
> "latest" is just `npm i -g <pkg>` / the nodejs.org `latest-lts` channel. The
> resolved concrete versions are written back into `manifest.lock` by the bake.
>
> **Status:** the auth/first-run behaviour below is what Phase 6 is built
> against. The cells marked `latest` are resolved at bake time; the verification
> recipe at the bottom confirms auth-from-env in a container matching the rootfs
> base (Ubuntu 24.04, x86_64) — that step needs a Linux host with network and is
> part of the Track-B bake, not this Mac/dev checkout. The control-plane
> contracts (registry shape, injector, setup_command runner, UI, e2e) are all
> green without it; only the actual image bake (6.3) and live pass (6.6) depend
> on the bake.

## Base capability: Node LTS

| field | value |
|---|---|
| why | shared runtime for the npm-distributed agents (Gemini, Codex, pi.dev) |
| install | tarball from `https://nodejs.org/dist/<ver>/node-<ver>-linux-<arch>.tar.xz`, sha256 verified against the dist `SHASUMS256.txt`, unpacked into `/usr/local` (`node`, `npm`, `npx` on PATH) |
| version | **latest LTS by default** (resolved from `nodejs.org/dist/latest-lts`); pin with `--node-version vX.Y.Z` |
| sha256 | recorded in `manifest.lock` after the bake; override with `--node-sha256 <hex>` |
| installed when | any npm provider CLI below is enabled |

## claude — Claude Code (Phase 5, restated)

| field | value |
|---|---|
| package | native binary from Anthropic's release endpoint (`downloads.claude.ai`) |
| install | `/usr/local/bin/claude` (pinned, sha256 in `manifest.lock`); `--claude-bootstrap` or `--claude-binary` |
| auth | **pure env** — `ANTHROPIC_API_KEY`. First-run "use this key?" pre-answered by `claude-managed-settings.json` (`/etc/claude-code/managed-settings.json`) |
| setup_command | none |
| first run | writes `~/.claude*` (durable once Phase 4 persistent home is mounted) |
| launch | `claude` |
| secret_fields | `[{name: api_key, label: "Anthropic API key", env: ANTHROPIC_API_KEY}]` |

## gemini — Gemini CLI

| field | value |
|---|---|
| package | `@google/gemini-cli` (npm, global) |
| install | `npm i -g @google/gemini-cli` via chroot (latest); `--gemini-version X.Y.Z` to pin |
| version | latest by default (resolved version recorded in `manifest.lock`) |
| auth | **pure env** — `GEMINI_API_KEY` (honoured for API-key auth; the OAuth/login flow is a non-goal, Phase 6 decision #6) |
| setup_command | none |
| first run | may show an interactive theme/onboarding picker in the PTY — acceptable (decision #6); writes `~/.gemini/` |
| launch | `gemini` |
| secret_fields | `[{name: api_key, label: "Gemini API key", env: GEMINI_API_KEY}]` |

## openai — OpenAI Codex

| field | value |
|---|---|
| package | `@openai/codex` (npm, global) — installed via the shared Node runtime |
| install | `npm i -g @openai/codex` via chroot (latest); `--codex-version X.Y.Z` to pin |
| version | latest by default (resolved version recorded in `manifest.lock`) |
| auth | **login step, not pure env**: `printenv OPENAI_API_KEY \| codex login --with-api-key` writes `~/.codex/auth.json`. Delivered as the registry `setup_command`, run by the guest agent on every push (idempotent: re-login overwrites) |
| setup_command | `printenv OPENAI_API_KEY \| codex login --with-api-key` |
| first run | writes `~/.codex/` (incl. `auth.json`, plaintext — acceptable: the user's own key in their own VM on the Phase 4 encrypted disk) |
| launch | `codex` |
| secret_fields | `[{name: api_key, label: "OpenAI API key", env: OPENAI_API_KEY}]` |

> Distribution note: Phase 6 decision #4 originally pinned Codex as a standalone
> musl binary (so it needed no Node). Since Node is now baked anyway for Gemini
> and pi.dev, Codex installs via npm (`@openai/codex`) for uniform
> latest-by-default across all four providers — the "no Node" rationale no longer
> applies. The auth mechanism (the `setup_command` login) is unchanged.
>
> Idempotency matters: `setup_command` runs on **every** push (start, resume,
> rotation). A non-idempotent login would corrupt state; `codex login
> --with-api-key` overwrites `auth.json`, so it is safe.

## pi — Pi (pi.dev)

| field | value |
|---|---|
| package | `@earendil-works/pi-coding-agent` (npm, global) — its `bin` is `pi`, matching the registry launch command. (Not `@oh-my-pi/pi-coding-agent`, which ships an `omp` binary.) |
| install | `npm i -g @earendil-works/pi-coding-agent` via chroot (latest); `--pi-version X.Y.Z` to pin |
| version | latest by default (resolved version recorded in `manifest.lock`) |
| auth | **borrowed model-provider key** — pi has no key of its own; it reads `ANTHROPIC_API_KEY` (or `OPENAI_API_KEY` / `GEMINI_API_KEY`) from env, or `~/.pi/agent/auth.json`. We inject **only** the Anthropic key, stored under **pi's own** secret path (never read from claude's — Phase 6 decision #2) |
| setup_command | none |
| first run | writes `~/.pi/` |
| launch | `pi` |
| headless | **yes** — runs on the AT1 task lane. The guest spawns `pi --mode json` (prompt on stdin) and parses its JSON event stream (session header → `message_*`/`tool_execution_*`/`turn_end` → terminal `agent_end`). Multi-turn resume targets a stored session by id via `--session <id>` (not `--resume`, an interactive picker). Claude is the other headless provider; gemini/codex are interactive-terminal only. |
| secret_fields | `[{name: anthropic_api_key, label: "Anthropic API key (used by Pi)", env: ANTHROPIC_API_KEY}]` |

## Image size

`TODO(bake)`: record final image size + build time after the first real bake.
Node (~120 MiB unpacked) + the three npm CLIs grow the image materially over the
Phase 5 claude-only image; `build-rootfs.sh` reserves +512 MiB headroom when any
provider CLI is baked.

## Verification recipe (run on Ubuntu 24.04 x86_64 with network)

For each CLI, in a container matching the rootfs base:

1. **Confirm the package/channel** (only needed when changing a pin):
   `npm view @google/gemini-cli version`, `npm view @openai/codex version`,
   `npm view @earendil-works/pi-coding-agent version`; Node via the
   `nodejs.org/dist/index.json` latest-LTS entry.
2. **Prove auth-from-env** with a dummy key — the call should fail *inside the
   CLI* (bad key), proving the env path is wired, not a config prompt:
   - `ANTHROPIC_API_KEY=sk-dummy claude -p hello` (and `GEMINI_API_KEY`/gemini, `ANTHROPIC_API_KEY`/pi)
   - Codex: `printenv OPENAI_API_KEY | codex login --with-api-key` then `codex -p hello`
3. **Record first-run files** written under `$HOME` (the persistent-home set).
4. **Record the launch command** (the bare CLI name above unless a flag is needed).

Then bake (latest by default; add `--<name>-version` only to pin):

```
image/build-rootfs.sh --base <firecracker-ci-ubuntu-24.04.ext4> \
  --claude-bootstrap \
  --gemini --codex --pi
```

The bake installs the latest Node LTS + the latest of each enabled CLI and writes
the resolved versions into `manifest.lock`. Re-pin `PROTEOS_ROOTFS_REF` to the
emitted image (see `RUNBOOK.md`). In Ansible the providers are **opt-in** (each
`proteos_<provider>_install` defaults to `none`); enable the ones you want — see
`deploy/ansible/group_vars/all.yml`.
