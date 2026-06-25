# ProteOS Architecture

A short tour of how ProteOS is put together. For local dev see the
[README](../README.md); for production deployment and operations see
[DEPLOYMENT.md](../DEPLOYMENT.md) and [RUNBOOK.md](../RUNBOOK.md). Design history
for each phase lives in [`plans/`](../plans/).

## Overview

ProteOS gives each user one or more **machines** — full
[Firecracker](https://firecracker-microvm.github.io/) microVMs with their own
kernel, rootfs, and workspace. AI coding agents (Claude Code, Gemini, Codex) run
*inside* those microVMs, so an agent is isolated from the host by a hardware VM
boundary rather than a shared kernel. Users drive their machines from an
ocean-themed browser desktop or from the `proteos` CLI.

The system is a Go workspace (`go.work`) of four modules plus a React SPA:

| Component        | Module/Path     | Runs on            | Role                                                              |
| ---------------- | --------------- | ------------------ | ----------------------------------------------------------------- |
| **Control plane**| `controlplane/` | app-stack host     | HTTP API, auth, Postgres, secrets, and the per-machine gateway    |
| **Node-agent**   | `nodeagent/`    | KVM host(s)        | Provisions & supervises Firecracker microVMs on a host            |
| **Guest-agent**  | `guestagent/`   | inside each microVM| Executes work in the guest, reachable over vsock                  |
| **CLI**          | `cli/`          | user's machine/CI  | `proteos` — scriptable client over the same HTTP API              |
| **Web**          | `web/`          | served via nginx   | React (Vite) single-page desktop                                  |

## Request path

```
browser ──► TLS proxy ──► web (nginx) ──► control plane ──► node-agent ──► Firecracker microVM
  / CLI                                       │  (HTTP/WS)    (on KVM host)    └─ guest-agent (vsock)
                                              └─ Postgres + OpenBao (secrets)
```

- The **control plane** is the only component clients talk to. It serves the
  REST/JSON API consumed by both the web SPA and the CLI (`--json`, stable exit
  codes), terminates user sessions/auth, and persists state in **Postgres**.
- It reaches a **node-agent** over HTTP/WebSocket to create, start, stop, and
  inspect microVMs. The node-agent owns its KVM host: IP/tap allocation, the
  jailer, nftables, and the Firecracker process lifecycle.
- Inside each microVM, the **guest-agent** listens on **vsock**. The control
  plane's **gateway** dials the guest through the node-agent to multiplex
  terminals, the code-server editor, and per-port previews back to the browser
  over a single authenticated `/gw/` WebSocket path.

## Key design points

- **Isolation boundary.** Each machine is a separate microVM with its own kernel
  — not a container. Cross-tenant isolation and guest-to-host containment are the
  project's core security properties (see [SECURITY.md](../SECURITY.md)).
- **vsock, not network, to the guest.** The guest-agent has no inbound network
  surface; the host↔guest control channel is vsock, and user-facing traffic is
  proxied through the authenticated gateway.
- **Secrets stay out of the environment.** Provider API keys and the GitHub
  connection are entered per-user in the desktop, stored encrypted in **OpenBao**,
  and injected into a machine on demand — never set as process env on the host.
- **The gateway is the single ingress to a machine.** Terminals, the in-browser
  VS Code editor (code-server), and port previews are all reached at
  per-machine / per-port subdomains through the gateway, so there is no
  unauthenticated path into a guest.
- **Pluggable VM driver.** The node-agent has a `dev` driver (default, no KVM —
  used for local development and most CI) and a `firecracker` driver behind a
  build tag (real microVMs, exercised only on KVM hosts).
- **Agent-first CLI.** `proteos` is built to be driven by a coding agent or CI as
  much as by a human, mirroring the same control-plane API the desktop uses.

## Control-plane internals

The control plane is organized into focused packages under
`controlplane/internal/`, including: `httpapi` (REST surface), `auth`/`session`
(authentication), `gateway`/`guestctl` (WebSocket multiplexing to the guest),
`machine`/`nodeclient` (orchestration and the node-agent client), `secrets`
(OpenBao), `providers`/`injector`/`profile` (per-user provider config injected
into machines), `github`/`token` (GitHub App and token sourcing), `store`
(Postgres / sqlc), and `audit`.

## Deployment shape

In production the control-plane app-stack (web nginx + control plane + Postgres,
see `deploy/app-stack/`) runs on one host behind a TLS proxy, while one or more
**KVM hosts** run the node-agent natively and host the microVMs (provisioned via
`deploy/ansible/` and `deploy/proxmox/`). The app-stack host does not need KVM;
the KVM hosts do not face the public internet. See
[DEPLOYMENT.md](../DEPLOYMENT.md) for the full topology.
