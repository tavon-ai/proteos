# 🌊 ProteOS (P/OS)

> *"Shape-shifting intelligence from the depths of containerization"*

**ProteOS** — derived from **Proteus (Πρωτεύς)**, the Greek sea god of shape-shifting, wisdom, and prophecy. Just as Proteus could transform into any form, ProteOS adapts seamlessly between multiple AI providers, embodying flexibility and intelligence while maintaining Docker's oceanic heritage.

![ProteOS](https://img.shields.io/badge/status-production-green) ![Docker](https://img.shields.io/badge/docker-required-blue) ![Node](https://img.shields.io/badge/node-20+-green) ![AI](https://img.shields.io/badge/AI-3%20providers-purple)

![Desktop Overview](images/desktop-overview.png)
*ProteOS ocean-themed desktop with multiple AI providers*

![Gemini Terminal](images/gemini-terminal.png)
*Gemini CLI running in a dedicated terminal window*

## ✨ Features

### 🎭 Multi-AI Provider Support
- **🐋 Claude Code** (Anthropic Claude 3.5 Sonnet)
- **🔷 Gemini CLI** (Google Gemini 2.5 Pro)
- **⚡ OpenAI Codex** (OpenAI GPT-4/Codex)

### 🐳 Docker-Powered
- **Isolated Containers**: Each AI runs in its own environment
- **Resource Efficient**: Only active containers consume resources
- **Easy Scaling**: Spawn unlimited AI instances

### 📁 File System
- **Persistent Storage**: Each container gets its own workspace
- **File editing**: handled by **code-server** (a full VS Code in the browser),
  reached through the authenticated gateway at the per-machine editor subdomain.
  The old unauthenticated PoC file-browser/viewer was removed in Phase 8
  (decision #7) — see `plans/phase-8-implementation.md`.
- **Easy Access**: Files stored locally in `workspace/containers/`

### 📊 System Monitoring
- **Live System Logs**: Dedicated window showing real-time operations and events
- **Log Filtering**: Filter logs by level (Info, Success, Warning, Error)
- **Auto-Scroll**: Optional automatic scrolling to latest log entries
- **Debug Visibility**: Track container creation, connections, and failures instantly

### 🌐 Web-Based
- **No Installation**: Access from any browser
- **Remote Access**: Run on server, access from anywhere
- **Cross-Platform**: Works on Mac, Linux, Windows


## 🚀 Quick Start (local development)

ProteOS is a Go control plane + node-agent (Firecracker microVMs) with a React
(Vite) single-page app. The original Node/Express + Docker proof-of-concept
(`server/`, `public/`) was **retired in Phase 9** — the Go control plane and the
React desktop are the whole application now.

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

See **DEPLOYMENT.md** for the production (Proxmox / app-stack) deployment and
**RUNBOOK.md** for operations.

## 🔑 API keys

Provider API keys (Claude, Gemini, OpenAI Codex) are no longer set via process
environment. They are entered per-user in the desktop **Settings → AI providers**
window, stored encrypted (OpenBao), and injected into the machine on demand. The
GitHub connection is managed from the same Settings window.
