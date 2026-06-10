# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

ProteOS is a web-based "desktop OS" that spawns AI coding CLIs (Claude Code, Gemini CLI, OpenAI Codex) inside isolated Docker containers and exposes each as a browser terminal. The orchestrator server manages the container lifecycle; the browser UI renders draggable windows whose contents are `ttyd` web terminals served from each container.

## Commands

```bash
npm start          # run the orchestrator server (node server/index.js)
npm run dev        # same, with --watch auto-reload
docker compose up --build   # run the whole stack in a container (Docker-in-Docker)
```

There are no tests, linter, or build step. The frontend is vanilla JS/CSS served statically — no bundler.

## Architecture

**Two layers, connected by the Docker socket:**

1. **Orchestrator** (`server/index.js`) — a single-file ESM Express server. It is the *only* backend code. On startup it connects to the Docker daemon and calls `ensureAllImages()`, which builds `proteos-claude`/`proteos-gemini`/`proteos-openai` images from the `dockerfile.<provider>` files if they don't already exist. It then serves the REST API and static `public/`.

2. **Per-provider AI containers** — built from `dockerfile.claude`, `dockerfile.gemini`, `dockerfile.openai`. Each installs its CLI plus `ttyd` and runs the CLI inside a web terminal on container port `7681`. The orchestrator creates these on demand, maps `7681` to an incrementing host port (starting at `7681`), injects the relevant API key as an env var, and bind-mounts a per-container workspace dir.

**Frontend** (`public/app.js`) is one `ProteOS` class implementing a windowing desktop. Clicking a provider icon POSTs to `/api/containers/create`; each window embeds an `<iframe>` pointed at `http://<hostname>:<container-port>` — i.e. the browser talks **directly** to each container's `ttyd`, not through the server. The server only orchestrates; it does not proxy terminal traffic (the WebSocket server in `index.js` is a vestigial stub).

### Provider registration
Adding/changing a provider means keeping three things in sync:
- `imageConfigs` map in `server/index.js` (image name, dockerfile, API-key env var)
- the matching `dockerfile.<provider>`
- the desktop icon (`data-container-type`) in `public/index.html` and its config in `public/app.js`

## Important gotchas

- **Container state is in-memory only.** The `containers` Map in `server/index.js` is the source of truth and is lost on server restart — orphaned Docker containers will keep running (they use `AutoRemove: true`, so they vanish when stopped, but the server forgets about them on restart). There is no reconciliation with actual Docker state.
- **Port inconsistency.** The server defaults to `PORT=3001` (and `compose.yml` uses 3001), but `README.md` and `DEPLOYMENT.md` still reference 3000 in several places. Trust the code: 3001.
- **Hardcoded socket fallback.** Docker connection falls back to `/Users/javieralonso/.docker/run/docker.sock` (a specific developer's path) if `/var/run/docker.sock` fails. Replace this rather than copy it.
- **Docker-in-Docker.** When running via `compose.yml`, the server container mounts the host Docker socket and uses `HOST_WORKSPACE_PATH` to translate bind-mount paths so child containers mount host directories, not paths inside the server container.
- **Workspace paths.** Per-container files live in `workspace/containers/<containerId>/` (gitignored). The file-browser endpoints guard against path traversal with a `startsWith(workspaceDir)` check — preserve that check when touching `/api/containers/:id/files*` or `/api/workspace/folders*`.
- **`POST /api/settings/api-keys` rewrites the `.env` file** on disk in addition to mutating `process.env`. Existing containers keep the key they were created with.
- **`/api/terminal/local` is macOS-only** (uses iTerm2 URL scheme / AppleScript).

## Security note

The server has full control of the Docker daemon via the mounted socket (effectively host root). This is intended for trusted/local environments only — see the security section in `DEPLOYMENT.md`.
