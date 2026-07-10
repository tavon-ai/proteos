# Contributing to ProteOS

Thanks for your interest in contributing! ProteOS is a self-hostable platform
for running AI coding agents in isolated Firecracker microVMs. This guide covers
how to set up a dev environment, the checks your change must pass, and how we
review pull requests.

By participating you agree to abide by our [Code of Conduct](CODE_OF_CONDUCT.md).

## Architecture at a glance

ProteOS is a Go workspace (`go.work`) plus a React SPA:

| Component        | Path            | Role                                                        |
| ---------------- | --------------- | ----------------------------------------------------------- |
| **control plane**| `controlplane/` | HTTP API, Postgres, auth, gateway, orchestration            |
| **node-agent**   | `nodeagent/`    | Runs on KVM hosts; provisions Firecracker microVMs          |
| **guest-agent**  | `guestagent/`   | Runs inside each microVM (vsock); executes work in the guest |
| **CLI**          | `cli/`          | `proteos` — drives the platform from the terminal/CI        |
| **web**          | `web/`          | React (Vite) single-page desktop                            |

Deployment and operations live in `deploy/`, `DEPLOYMENT.md`, `RUNBOOK.md`, and
`INCIDENT_RUNBOOK.md`. Design history is in `plans/`.

## Prerequisites

- **Go** — version pinned in the per-module `go.mod` (see `go.work`)
- **Node** 25+ and npm (for `web/`)
- **Docker** (or compatible) — for the local dev Postgres
- **[Task](https://taskfile.dev)** — the task runner (`Taskfile.yaml`)
- Optional: a Linux host with **KVM** + Firecracker/jailer to exercise the
  Firecracker driver (the dev driver is the default and needs none of this)

## Local development

```bash
# One-time: install local tooling (sqlc) and web deps
task setup

# Bring up the dev database (Postgres)
task dev:db          # or: docker compose -f compose.dev.yml up -d

# Run the stack in separate terminals
task na:run          # node-agent (dev driver)
task cp:run          # control plane (depends on Postgres + node-agent)
task web:dev         # Vite dev server, proxies /api and /gw to the control plane

# Open the SPA
open http://localhost:5173
```

Run `task --list` for every available target.

## Before you open a PR

Your change must pass the same checks CI runs. The quickest way to mirror CI
locally is:

```bash
task ci              # control plane + node-agent + guest-agent + web
```

Or run the pieces you touched:

```bash
task test            # all Go + web tests (race detector on)
task vet             # go vet across all modules (incl. the linux firecracker build)
task fmt             # format Go + web
task fmt:check       # fail if anything is not formatter-clean
task web:lint        # ESLint for the web app
```

Notes:

- **Go control-plane tests** use Testcontainers for Postgres locally (no setup),
  or set `TEST_DATABASE_URL` to point at an existing database.
- If you change SQL queries, regenerate with `task cp:sqlc` and make sure
  `task cp:sqlc:diff` is clean — CI fails on sqlc drift.
- The **Firecracker driver** is behind a build tag and only tested on KVM hosts.
  CI runs those tests only on a self-hosted KVM runner; you don't need KVM for
  most changes.

## Pull request guidelines

- **Branch** off `main` and open a PR against `main`.
- Keep PRs focused; one logical change per PR is easier to review.
- Write a clear description: what changed, why, and how you tested it.
- Update docs (`README.md`, `DEPLOYMENT.md`, `RUNBOOK.md`, `INCIDENT_RUNBOOK.md`,
  `docs/`) when behavior or operations change.
- Add or update tests for new behavior.
- Make sure `task ci` is green and the working tree is formatter-clean.

### Commit messages

We use [Conventional Commits](https://www.conventionalcommits.org/)-style
prefixes (`feat:`, `fix:`, `chore:`, `docs:`, `refactor:`, `test:`), matching the
existing history.

## Reporting bugs and requesting features

Use the GitHub issue templates. For anything that looks like a **security
vulnerability**, do not open a public issue — follow [SECURITY.md](SECURITY.md)
instead.

## License

By contributing, you agree that your contributions will be licensed under the
[MIT License](LICENSE) that covers this project.
