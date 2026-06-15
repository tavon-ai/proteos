# Provider CLIs — install, auth, first-run (Phase 6 task 6.0)

This file is the **single source of truth for provider pins**. No version literal
lives in the migration or `build-rootfs.sh`; every version/sha here is what the
bake (`build-rootfs.sh`) and the registry seeds (`migration 000005`) are pinned
to. When a CLI is upgraded, change it here first, then re-bake and re-seed.

Each row records: how the CLI is installed into the rootfs, how it authenticates
(the contract is *auth-from-injected-env*, optionally via a registry
`setup_command` — never a browser-visible secret prompt), what it writes under
`$HOME` on first run, and the launch command the registry stores.

> **Status:** the auth/first-run behaviour below is what Phase 6 is built
> against. The exact `version`/`sha256` cells marked `TODO(bake)` must be filled
> by running the verification recipe at the bottom in a container matching the
> rootfs base (Ubuntu 24.04, x86_64) — that step needs a Linux host with network
> and is part of the Track-B bake, not this Mac/dev checkout. The control-plane
> contracts (registry shape, injector, setup_command runner, UI, e2e) are all
> green without it; only the actual image bake (6.3) and live pass (6.6) depend
> on these pins.

## Base capability: Node LTS

| field | value |
|---|---|
| why | shared runtime for npm-distributed agents (Gemini, pi.dev) |
| install | pinned tarball from `https://nodejs.org/dist/<ver>/node-<ver>-linux-<arch>.tar.xz`, sha256 verified against the dist `SHASUMS256.txt`, unpacked into `/usr/local` (`node`, `npm`, `npx` on PATH) |
| version | `TODO(bake)` — pin an active LTS (e.g. `v22.x`) |
| sha256 | `TODO(bake)` |
| flag | `--node-version vX.Y.Z [--node-sha256 <hex>]` |

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
| package | `@google/gemini-cli` (npm, global, pinned) |
| install | `npm i -g @google/gemini-cli@<ver>` via chroot into the image |
| version | `TODO(bake)` |
| auth | **pure env** — `GEMINI_API_KEY` (honoured for API-key auth; the OAuth/login flow is a non-goal, Phase 6 decision #6) |
| setup_command | none |
| first run | may show an interactive theme/onboarding picker in the PTY — acceptable (decision #6); writes `~/.gemini/` |
| launch | `gemini` |
| secret_fields | `[{name: api_key, label: "Gemini API key", env: GEMINI_API_KEY}]` |

## openai — OpenAI Codex

| field | value |
|---|---|
| package | static musl binary (no Node) from the Codex release artifacts |
| install | `/usr/local/bin/codex` (pinned, sha256 in `manifest.lock`); `--codex-binary` / `--codex-url` + `--codex-version` |
| version | `TODO(bake)` |
| auth | **login step, not pure env**: `printenv OPENAI_API_KEY \| codex login --with-api-key` writes `~/.codex/auth.json`. Delivered as the registry `setup_command`, run by the guest agent on every push (idempotent: re-login overwrites) |
| setup_command | `printenv OPENAI_API_KEY \| codex login --with-api-key` |
| first run | writes `~/.codex/` (incl. `auth.json`, plaintext — acceptable: the user's own key in their own VM on the Phase 4 encrypted disk) |
| launch | `codex` |
| secret_fields | `[{name: api_key, label: "OpenAI API key", env: OPENAI_API_KEY}]` |

> Idempotency matters: `setup_command` runs on **every** push (start, resume,
> rotation). A non-idempotent login would corrupt state; `codex login
> --with-api-key` overwrites `auth.json`, so it is safe.

## pi — Pi (pi.dev)

| field | value |
|---|---|
| package | the pi.dev coding agent (npm, global, pinned) — confirm the exact package name at bake (`@pi/agent` assumed; `build-rootfs.sh` and the table must match) |
| install | `npm i -g <pkg>@<ver>` via chroot |
| version | `TODO(bake)` |
| auth | **borrowed model-provider key** — pi has no key of its own; it reads `ANTHROPIC_API_KEY` (or `OPENAI_API_KEY` / `GEMINI_API_KEY`) from env, or `~/.pi/agent/auth.json`. We inject **only** the Anthropic key, stored under **pi's own** secret path (never read from claude's — Phase 6 decision #2) |
| setup_command | none |
| first run | writes `~/.pi/` |
| launch | `pi` |
| secret_fields | `[{name: anthropic_api_key, label: "Anthropic API key (used by Pi)", env: ANTHROPIC_API_KEY}]` |

## Image size

`TODO(bake)`: record final image size + build time after the first real bake.
Node (~120 MiB unpacked) + the npm CLIs + Codex grow the image materially over
the Phase 5 claude-only image; `build-rootfs.sh` reserves +512 MiB headroom when
any provider CLI is baked.

## Verification recipe (run on Ubuntu 24.04 x86_64 with network)

For each new/updated CLI, in a container matching the rootfs base:

1. **Pin the artifact.** Resolve the exact version and record its sha256 here:
   - Node: `curl -fsSL https://nodejs.org/dist/<ver>/SHASUMS256.txt | grep linux-x64.tar.xz`
   - Codex: download the pinned release binary, `sha256sum` it.
   - npm CLIs: `npm view <pkg> version` to pin, install with the exact `@<ver>`.
2. **Prove auth-from-env** with a dummy key — the call should fail *inside the
   CLI* (bad key), proving the env path is wired, not a config prompt:
   - `ANTHROPIC_API_KEY=sk-dummy claude -p hello` (and `GEMINI_API_KEY`/gemini, `ANTHROPIC_API_KEY`/pi)
   - Codex: `printenv OPENAI_API_KEY | codex login --with-api-key` then `codex -p hello`
3. **Record first-run files** written under `$HOME` (the persistent-home set).
4. **Record the launch command** (the bare CLI name above unless a flag is needed).

Then bake, e.g.:

```
image/build-rootfs.sh --base <firecracker-ci-ubuntu-24.04.ext4> \
  --claude-bootstrap \
  --node-version vX.Y.Z \
  --gemini-version A.B.C \
  --pi-version D.E.F \
  --codex-binary ./codex --codex-version G.H.I --codex-sha256 <hex>
```

and re-pin `PROTEOS_ROOTFS_REF` to the emitted image (see `RUNBOOK.md`).
