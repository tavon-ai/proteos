# 🌊 ProteOS (P/OS)

> *"Shape-shifting intelligence from the depths of virtualization"*

**ProteOS** is a self-hostable platform for running headless AI coding agents in
strongly isolated, per-user **Firecracker microVMs**. Create a machine, clone a
repo into it, and dispatch a coding-agent task — the agent edits files inside
its own disposable VM, and you review the diff, commit, push, and open a PR
either from the web UI or from the `proteos` CLI.

Autonomous coding agents are powerful but risky to run loose on a laptop or a
shared build box. ProteOS gives each agent a disposable, hardware-isolated
microVM with its own kernel, workspace, and exposed ports, so you can run many
agents in parallel — safely — and either watch them through an ocean-themed
browser desktop or drive the whole loop headlessly from a CLI and CI.

The name is derived from **Proteus (Πρωτεύς)**, the Greek sea god of
shape-shifting, wisdom, and prophecy. Just as Proteus could transform into any
form, ProteOS adapts between multiple AI providers while keeping each one
isolated in its own Firecracker microVM.

![ProteOS](https://img.shields.io/badge/status-production-green) ![Go](https://img.shields.io/badge/go-control--plane-blue) ![Firecracker](https://img.shields.io/badge/firecracker-microVMs-orange) ![React](https://img.shields.io/badge/react-SPA-61dafb) ![AI](https://img.shields.io/badge/AI-3%20providers-purple)

![Desktop Overview](images/desktop-overview.png)
*ProteOS ocean-themed desktop, one window per machine and provider*

![Gemini Terminal](images/gemini-terminal.png)
*A coding agent running in a dedicated terminal window on a machine*

## Architecture

ProteOS is a Go workspace of four modules plus a React SPA:

- **Control plane** (`controlplane/`) — the only component clients talk to.
  Go HTTP API, auth, Postgres-backed state, secrets (OpenBao), and the
  **gateway**: the single ingress into a machine, multiplexing terminal,
  editor, and port-preview traffic over one `/gw/` WebSocket.
- **Node-agent** (`nodeagent/`) — runs on a KVM host and owns its Firecracker
  microVMs through a pluggable driver: a `dev` driver (no KVM required, used
  for local development and CI) and a real `firecracker` driver for
  production. Handles machine lifecycle, LUKS-encrypted volumes, and network
  isolation for each microVM.
- **Guest-agent** (`guestagent/`) — runs inside every microVM and is reachable
  only over vsock; the guest has no inbound network access. It executes
  coding-agent tasks and git operations inside the machine's workspace.
- **CLI** (`cli/`) — the `proteos` binary, a scriptable client for the control
  plane's API. Built to be driven by a coding agent as much as by a human:
  most read commands support `--json` and exit codes are stable.
- **Web UI** (`web/`) — a React (Vite) single-page app: an ocean-themed
  desktop for humans, plus a lightweight mobile shell for reviewing PRs from
  a phone.

Each machine is a separate microVM with its own kernel — not a shared-kernel
container — so a compromised or runaway agent can't reach the host, other
machines, or the internet except through the ports the control plane
explicitly exposes. See **[docs/architecture.md](docs/architecture.md)** for
the full write-up and **[RUNBOOK.md](RUNBOOK.md)** for day-2 operations.

## Key capabilities

- **Machine lifecycle** — create a microVM from a template, list your
  machines, start and stop them on demand. Resources (vCPUs, memory, disk)
  are only consumed while a machine is running.
- **Task dispatch (headless coding agents)** — send a prompt to a project
  inside a machine and a headless agent (Claude Code, with more providers
  pluggable) edits the code. Poll, watch the live event stream, or cancel a
  running task. The agent never commits on its own — it leaves a dirty
  working tree for you to review.
- **Git integration** — inspect status and diffs, create branches, commit
  with an explicit message (the review gate before anything reaches origin),
  push a branch upstream, and open a pull request against the repo's default
  branch — all against the repo checked out inside the machine.
- **SSH / web terminal** — a full xterm.js terminal over WebSocket into any
  machine, used both for interactive shell access and for watching an agent
  work in real time. File editing is handled by **code-server** (VS Code in
  the browser) reached through the authenticated gateway.

## Supported templates

Machines are created from a template that picks the rootfs image and default
resources (all overridable per machine within configured caps):

| Template | Label | Contents | Defaults |
| --- | --- | --- | --- |
| `base` | Base | Platform baseline — editor, shell, git | 2 vCPU / 2048 MiB / 10 GiB disk |
| `go` | Go development | Go toolchain + Taskfile | 2 vCPU / 2048 MiB / 10 GiB disk |
| `node` | Node.js development | Node LTS + npm provider CLIs | 2 vCPU / 2048 MiB / 10 GiB disk |
| `python` | Python development | Python 3 + pip + venv + build tools | 2 vCPU / 2048 MiB / 10 GiB disk |
| `full` | Full stack (Go + Node + Python) | Go, Node, and Python with build essentials | 4 vCPU / 4096 MiB / 20 GiB disk |

Every template shares a common platform layer baked into the rootfs image:
the guest-agent binary, git, vim, Taskfile, GitHub CLI (`gh`), and the Claude
Code CLI. The catalog lives in `deploy/app-stack/proteos-templates.json`
(overridable via `PROTEOS_TEMPLATES_FILE`); resource caps are set with
`PROTEOS_MAX_VCPUS` / `PROTEOS_MAX_MEM_MIB` / `PROTEOS_MAX_DISK_MIB`.

## Command-line interface

`proteos` drives ProteOS from the terminal — manage machines, dispatch
headless coding-agent tasks, and review/commit/push the results.

```bash
# Machines
proteos machines create --name my-box --template full --vcpus 4 --mem-mib 8192
proteos machines ls --json
proteos machines start m-123
proteos machines stop m-123

# Tasks (headless coding agents)
proteos task run --machine m-123 --project myrepo "add a health check endpoint"
proteos task run --machine m-123 --project myrepo --watch "fix the failing test"
proteos task get --machine m-123 t-456 --json

# Git — status, diff, commit, push, PR
proteos git status --machine m-123 --project myrepo
proteos git diff --machine m-123 --project myrepo
proteos git commit --machine m-123 --project myrepo -m "add health check"
proteos git push --machine m-123 --project myrepo --branch fix/login --set-upstream
proteos git pr --machine m-123 --project myrepo --head fix/login --title "Fix login"
```

Nearly every read command accepts `--json`, and exit codes are stable (`0` ok,
`2` usage, `3` auth, `4` not found, `5` task failed/canceled) so the CLI can
be driven from scripts and CI, not just interactively. See
**[cli/README.md](cli/README.md)** for install, authentication, and the full
command reference.

## Web interface

The web UI is an ocean-themed desktop: each machine gets its own window, and
each window can open sub-windows for the tools below.

- **Desktop** — the main shell (`web/src/desktop/Desktop.tsx`). A live,
  SSE-driven list of your machines; open, arrange, and close per-machine
  windows here.
- **Machine detail** — a metadata view for a single machine
  (`MachineDetails.tsx`): template, resource allocation, rootfs image, guest
  IP, and current state, plus start/stop controls.
- **Terminal** — an xterm.js terminal over WebSocket into the machine
  (`components/Terminal.tsx`), used both for an interactive shell and for
  watching a coding agent work live.
- **Editor** — an embedded code-server (full VS Code in the browser) reached
  through the authenticated gateway at the machine's editor subdomain
  (`windows/EditorWindow.tsx`).
- **Tasks & logs** — the tasks window (`windows/TasksWindow.tsx`) creates and
  lists coding-agent tasks and streams their live event log over SSE; a
  separate logs window (`windows/LogsWindow.tsx`) shows the machine's
  operational event feed, filterable by level (info/success/warning/error).
- **Changes** — a git review surface (`windows/ChangesWindow.tsx`): diff,
  stage, commit, push, and open a PR without leaving the browser.
- **Preview** — forwards and opens a machine's listening ports through the
  gateway at a per-machine, per-port subdomain (`windows/PreviewWindow.tsx`).
- **Settings** — per-user configuration (`windows/SettingsWindow.tsx`): AI
  provider API keys, GitHub connection, SSH keys, and download links.

A separate lightweight mobile shell (`web/src/mobile/`) exposes just a
machines list and a PR review view, for reviewing agent output from a phone.

## Deployment

Production deployment uses two machines and Docker Compose for everything
except the node-agent, which needs `/dev/kvm` and root and can't be
containerized:

- **App VM** — `deploy/app-stack/compose.yaml` runs `postgres` (state),
  `openbao` + a `bao-unsealer` sidecar (secrets, auto-unsealed on restart),
  `controlplane`, and `web` (the SPA served behind nginx).
- **KVM host(s)** — the native `proteos-node-agent` binary, deployed from
  `deploy/node-agent/`, talking to the app VM over the LAN.

`controlplane` and `web` run **pre-built images published to GHCR**
(`ghcr.io/tavon-ai/proteos-api`, `ghcr.io/tavon-ai/proteos-ui`) rather than
building from source at deploy time. CI (`.github/workflows/ci.yml`) publishes
both images together on every push to `main` (tags `latest` and
`sha-<commit>`) and on `v*.*.*` tags (semver). Pin `PROTEOS_VERSION` to a SHA
or semver tag in production — never `latest` — so a deploy is reproducible and
rollback is just resetting that one value:

```bash
cd deploy/app-stack
cp .env.example .env
$EDITOR .env   # set PROTEOS_VERSION=sha-a1b2c3d (or a semver tag)
docker compose up -d
# browse to $PROTEOS_BASE_URL and sign in with GitHub
```

Key environment variables (see `deploy/app-stack/.env.example` and
`deploy/node-agent/.env.example` for the full list):

| Variable | Purpose |
| --- | --- |
| `PROTEOS_VERSION` | Image tag pinned for both `controlplane` and `web` |
| `PROTEOS_BASE_URL` | Public URL the SPA and gateway are served from |
| `PROTEOS_SECRETS_BACKEND`, `PROTEOS_OPENBAO_*` | Secrets backend config (OpenBao) |
| `PROTEOS_NODE_AGENT_URL`, `PROTEOS_AGENT_TOKEN` | How the control plane reaches the node-agent |
| `PROTEOS_KERNEL_REF`, `PROTEOS_ROOTFS_REF`, `PROTEOS_TEMPLATES_FILE` | Machine image and template catalog |
| `PROTEOS_MAX_VCPUS`, `PROTEOS_MAX_MEM_MIB`, `PROTEOS_MAX_DISK_MIB` | Per-machine resource caps |
| `GITHUB_APP_CLIENT_ID`, `GITHUB_APP_CLIENT_SECRET`, `ALLOWED_GITHUB_LOGINS` | GitHub sign-in and access control |
| `PROTEOS_AGENT_DRIVER`, `PROTEOS_FIRECRACKER_BIN`, `PROTEOS_JAILER_BIN`, `PROTEOS_CRYPTSETUP_BIN` | Node-agent: Firecracker driver and LUKS volume support |

Provider API keys (Claude, and any other configured coding-agent provider)
are **not** set via process environment — they're entered per-user in the web
UI's Settings window, stored encrypted in OpenBao, and injected into a
machine only when a task runs on it.

See **[DEPLOYMENT.md](DEPLOYMENT.md)** for the full deploy/rollback
procedure, **[RUNBOOK.md](RUNBOOK.md)** for first-time setup and operations,
and **[docs/architecture.md](docs/architecture.md)** for how the pieces fit
together.

## Local development

```bash
# 1. Bring up the dev database (Postgres).
task dev:db          # or: docker compose -f compose.dev.yml up -d

# 2. Run the stack (separate terminals): control plane, node-agent, web SPA.
task na:run          # node-agent
task cp:run          # control plane (depends on Postgres + node-agent)
task web:dev         # Vite dev server (proxies /api and /gw to the control plane)

# 3. Open the SPA.
open http://localhost:5173
```

Run `task --list` for every target (build, test, vet, fmt).

## Contributing

Contributions are welcome! See **[CONTRIBUTING.md](CONTRIBUTING.md)** for dev
setup, the checks your change must pass, and PR guidelines, and please review
our **[Code of Conduct](CODE_OF_CONDUCT.md)**. For security issues, follow
**[SECURITY.md](SECURITY.md)** — do not open a public issue.

## License

ProteOS is released under the [MIT License](LICENSE).
