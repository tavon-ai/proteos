# proteos — the ProteOS CLI

`proteos` drives ProteOS from the command line: it lists your machines, clones
repositories onto them, runs headless coding-agent tasks, and reviews/commits the
results. It is designed to be driven by a coding agent as much as by a human — every
read command supports `--json`, and exit codes are stable and documented.

It talks to the control-plane HTTP API and authenticates with a personal access token
(`Authorization: Bearer`).

## Install

Build from this module:

```sh
cd cli
go build -o proteos ./cmd/proteos
# optionally: install onto your PATH
go install ./cmd/proteos
```

To stamp a version into the binary:

```sh
go build -ldflags "-X main.version=$(git describe --tags --always)" -o proteos ./cmd/proteos
```

## Authenticate

Mint a token in the browser under **Settings → CLI tokens**, then log in:

```sh
proteos auth login --url https://proteos.example.com --token <token>
# or pipe the token on stdin (it is not echoed):
proteos auth login --url https://proteos.example.com   # then paste the token
```

`auth login` verifies the token against `GET /api/me` and stores it. Check or clear it
with:

```sh
proteos auth status
proteos auth logout
```

## Configuration

The endpoint and token are resolved with this precedence (highest first):

1. Flags — `--url` (per-command)
2. Environment — `PROTEOS_URL`, `PROTEOS_TOKEN`
3. Stored credentials — `$XDG_CONFIG_HOME/proteos/credentials.json`, else
   `~/.config/proteos/credentials.json` (mode `0600`)

So an agent in CI can skip `auth login` entirely and just export `PROTEOS_URL` and
`PROTEOS_TOKEN`.

## The agent journey

The intended flow for "work on repo X with task Y" is:

```sh
# 1. Find an available machine and note its id + template (type).
proteos machines ls

# 2. Make sure the repo is on the machine (clones it if missing, no-op if present).
proteos project ensure --machine m-123 octocat/hello-world

# 3. Run the task and watch it.
proteos task run --machine m-123 --project hello-world --watch \
    "add a /health endpoint and a test for it"

# 4. Review and ship the agent's changes.
proteos git status  --machine m-123 --project hello-world
proteos git branch  --machine m-123 --project hello-world feature/health
proteos git commit  --machine m-123 --project hello-world -m "add /health endpoint"
proteos git push    --machine m-123 --project hello-world --branch feature/health --set-upstream
proteos git pr      --machine m-123 --project hello-world --head feature/health \
    --title "Add /health endpoint"
```

`project ensure` is idempotent — call it before every `task run` and it only clones when
the repo isn't already in the machine's workspace.

> **Claude on subscription.** The headless task lane runs Claude Code. If you have an
> Anthropic API key, store it under Settings → providers and it is injected into the
> machine. If you don't, tasks still run using the Claude subscription baked into the
> machine image — no `ANTHROPIC_API_KEY` required.

## Commands

### Machines & templates

```sh
proteos machines ls                 # id, name, state, template (type)
proteos machines get <id>           # one machine, including its template
proteos templates ls                # the machine types you can create (full-stack, go, …)
```

### Repositories & projects

```sh
proteos repo ls                                  # GitHub repos you can clone (owner/repo)
proteos project ls     --machine <id>            # repos cloned on a machine
proteos project clone  --machine <id> owner/repo # clone (async; add --wait to block)
proteos project ensure --machine <id> owner/repo # clone only if not already present
```

`clone` is asynchronous and returns an op id immediately; `--wait` (and `ensure`) poll
the project list until the repo appears.

### Tasks (headless coding agent)

```sh
proteos task run    --machine <id> --project <name> "<prompt>"   # dispatch (add --watch/--wait)
proteos task ls     --machine <id>                               # list tasks
proteos task get    --machine <id> <task-id>                     # status + result
proteos task watch  --machine <id> <task-id>                     # stream live events
proteos task cancel --machine <id> <task-id>                     # cancel (or --all-running)
proteos task send   --machine <id> <task-id> "<follow-up>"       # resume the agent session
```

The prompt may be given as arguments, read from a file with `--prompt-file`, or piped via
`--prompt-file -`. A task leaves a dirty working tree and stops — it never commits; use
the `git` commands for that.

### Git (review → commit → push → PR)

```sh
proteos git status --machine <id> --project <name>
proteos git diff   --machine <id> --project <name> [--staged]
proteos git branch --machine <id> --project <name> [--from <ref>] [--no-checkout] <name>
proteos git commit --machine <id> --project <name> -m "<message>" [paths...]
proteos git push   --machine <id> --project <name> --branch <b> [--set-upstream]
proteos git pr     --machine <id> --project <name> --head <b> --title "<t>" [--base <b>] [--body "<s>"]
```

## JSON output

Every read command (and most writes) accept `--json` for machine consumption. `task watch
--json` emits one normalized event per line (NDJSON), ideal for piping to `jq`:

```sh
proteos task watch --machine m-123 t-456 --json | jq .
```

## Exit codes

| Code | Meaning                          |
|------|----------------------------------|
| 0    | success                          |
| 1    | generic/runtime error            |
| 2    | usage / bad invocation           |
| 3    | authentication (HTTP 401/403)    |
| 4    | not found (HTTP 404)             |
| 5    | a task ended failed or canceled  |

Run `proteos <command> -h` for any command's flags and examples.
