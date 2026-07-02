# `proteos` CLI

The `proteos` command-line client drives the headless **Agent Task lane** over the
control-plane HTTP API: dispatch a coding-agent task against a repo cloned in a
machine, watch its structured event stream, cancel it, and send follow-up turns —
all without a browser. It is the programmatic surface a mobile app's own agent (or
a human in a terminal) uses to delegate work to ProteOS.

The agent run produces a **dirty working tree and stops there.** The CLI never
commits or pushes; that is the separate git-review surface.

## Install

```sh
# Build into cli/dist/proteos
task cli:build

# Or install into $GOBIN
task cli:install

# Cross-compiled release binaries (darwin/linux, amd64/arm64) → cli/dist/
task cli:cross
```

`proteos version` prints the build version.

## Authenticate

Authentication uses a **personal access token (PAT)**. Tokens are managed from the
browser:

1. Open ProteOS in the browser and go to **Settings → CLI tokens**.
2. **Create token**, give it a name, and **copy the value** — it is shown only
   once and cannot be recovered later. You can revoke it and create a fresh one
   from the same page at any time.
3. Give the token to the CLI, either by storing it:

   ```sh
   proteos auth login --url https://proteos.example.com
   # paste the token when prompted (it stays out of your shell history)
   ```

   …or, for an agent / CI, via environment variables (no `login` needed):

   ```sh
   export PROTEOS_URL=https://proteos.example.com
   export PROTEOS_TOKEN=proteos_pat_xxxxxxxx
   ```

`auth login` stores `{base_url, token, login}` in `~/.config/proteos/credentials.json`
(respecting `$XDG_CONFIG_HOME`) with `0600` permissions. The token is sent as
`Authorization: Bearer <token>` and never logged.

```sh
proteos auth status   # show endpoint + login (never the token)
proteos auth logout   # remove stored credentials
```

### Configuration precedence

| Value    | Resolution order                                  |
| -------- | ------------------------------------------------- |
| Endpoint | `--url` flag → `PROTEOS_URL` → stored `base_url`   |
| Token    | `PROTEOS_TOKEN` → stored `token`                   |

## Commands

```
proteos machines ls                 # list your machines
proteos machines get <id>           # show one machine
proteos machines create --template <id> [--name <name>]  # create a machine
proteos machines start <id>         # start a stopped machine
proteos machines stop <id>          # stop a running machine

proteos task run --machine <id> --project <repo> [--provider claude] "<prompt>"
proteos task ls   --machine <id>
proteos task get  --machine <id> <tid>
proteos task watch --machine <id> <tid>
proteos task cancel --machine <id> <tid>
proteos task send --machine <id> <tid> "<prompt>"
```

Every read command takes `--json` to emit the raw API JSON for scripting. Run
`proteos <command> -h` for the full flag list.

### Dispatch a task and watch it

```sh
proteos task run \
  --machine m-1234 \
  --project myrepo \
  --provider claude \
  --watch \
  "Add a health check endpoint and a test for it"
```

`--watch` streams normalized events (`assistant_text`, `tool_use`, `tool_result`,
and the terminal `result`) as the agent works, reconnecting automatically with
`Last-Event-ID` if the connection drops. `--json` turns the stream into NDJSON
(one event per line) for an agent consumer. Use `--wait` instead of `--watch` to
poll silently until the task reaches a terminal state.

The prompt may be passed as an argument, read from a file with
`--prompt-file <path>`, or piped via `--prompt-file -`.

### Follow-up turn

```sh
proteos task send --machine m-1234 t-5678 --watch "now also update the docs"
```

This resumes the agent's session for that task. It fails with `no_session` if the
task never captured a session, or `task_running` if a turn is still in flight.

### Cancel

```sh
proteos task cancel --machine m-1234 t-5678      # one task (idempotent)
proteos task cancel --machine m-1234 --all-running
```

Cancelling leaves whatever partial changes exist in the working tree for review.

## Exit codes

| Code | Meaning                                              |
| ---- | --------------------------------------------------- |
| 0    | success                                             |
| 1    | generic / runtime error                             |
| 2    | usage error (bad invocation)                        |
| 3    | authentication error (401 / 403)                    |
| 4    | not found (404)                                     |
| 5    | a task ended `failed` or `canceled`                 |

`task run --wait` / `--watch` exit `0` on `done` and `5` on `failed`/`canceled`,
so scripts can branch on the agent's outcome.
