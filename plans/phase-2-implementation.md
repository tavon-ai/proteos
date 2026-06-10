# Phase 2 Implementation Plan: Provision a Firecracker microVM (lifecycle)

> Source: `plans/proteos-poc-to-prod.md` Phase 2, planned 2026-06-10. Status: **not started**.

## Context

Phase 1 shipped the control-plane skeleton (Go + Postgres + GitHub auth) and the React SPA, leaving exact seams for Phase 2: `POST /api/machine`, `/start`, `/stop` are registered behind `requireAuth`+`csrfHeader` but return 501 (`controlplane/internal/httpapi/server.go`); `GET /api/me` returns a hardcoded `machine: null` (`handlers.go`); the Dashboard has a disabled "Create machine" button. Phase 2 makes machines real: a new **node-agent** boots jailed Firecracker microVMs behind a driver interface, the control plane owns the machine state machine + `machine_events` audit, and the dashboard shows live state over SSE. No terminal (Phase 3), no persistent disk/hibernate (Phase 4) — `stop` is a plain shutdown this phase.

**Decisions locked (2026-06-10):**
- **Parallel tracks**: dev-driver + control-plane work proceeds on the Mac immediately; the (scaffolded, not-yet-run) `spike/firecracker/` runs on Proxmox in parallel; the real Firecracker driver is the final task, written against spike findings.
- **Raw Firecracker API** (HTTP over unix socket, per the spike's `lib.sh`) — not firecracker-containerd.

## Architecture decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | **Separate Go module `nodeagent/`** + committed root `go.work`. Wire contract (types + route consts) lives in `nodeagent/api` (package `agentapi`), imported by controlplane; nodeagent never imports controlplane. | Privileged host daemon with a different deploy story; keeps Firecracker/netlink deps out of controlplane's go.sum. Scales to `guestagent/` in Phase 3 without `replace` directives. |
| 2 | **Control plane dials node-agent only** (HTTP/JSON on a private addr). Commands are synchronous "202 accepted"; **status flows back via control-plane polling** (2s poller for transitional states + 30s sweep of `running`). Auth: shared bearer token `PROTEOS_AGENT_TOKEN` on both sides, constant-time compare. Node-agent persists per-VM state on disk and re-attaches (or marks dead) on restart. | One dial direction = one credential, no callback route on the public API. The poller is the embryo of Phase 11's reconciliation loop. Bearer token is the simplest "authenticated, not open on the network"; mTLS swaps in at the middleware in Phase 11 without API changes. |
| 3 | **State machine enforced in one place**: `controlplane/internal/machine` — state consts, allowed-transitions table, and a `Transition` helper doing a guarded CAS `UPDATE machines SET state=$to WHERE id=$id AND state=$from` **plus** the `machine_events` insert **in the same pgx tx**. Async provisioning advances via the poller; failures → `error` with reason (event payload + `machines.last_error`). | Illegal transitions and missing audit rows are impossible by construction; no job queue needed for a single control-plane instance. |
| 4 | **Driver interface**: `EnsureRunning(ctx, VMSpec)`, `Stop`, `Status`, `Destroy`, `List`. `VMSpec{MachineID, Vcpus, MemMiB, KernelRef, RootfsRef, Net, Disks}` (`Disks` empty until Phase 4; Snapshot/Resume methods added then — no signature churn). **DevDriver is process-backed**: execs a real long-lived stub child per "VM", configurable boot delay, failure injection via `kernel_ref == "dev:fail-boot"`, same on-disk state files as the real driver. | `EnsureRunning` is the idempotent desired-state verb the poller wants. A real child process makes DevDriver honest about liveness checks and re-attach-after-restart — the same code paths Firecracker needs. |
| 5 | **SSE via in-process pubsub broker** (`machine/broker.go`); publish after commit. On connect: `snapshot` event (machine + last 50 events), then live `machine` events with `id:` = `machine_events.id` (bigserial); `Last-Event-ID` replays missed rows from DB on reconnect; heartbeat comment every 25s. | Proportionate for one control-plane instance / one dashboard; LISTEN/NOTIFY deferred to Phase 11. Bigserial id makes lossless replay free. |
| 6 | **Node-agent allocates IP/tap**: lowest free IP from a per-host subnet (default `172.30.0.0/24`, gateway `.1`), persisted in agent on-disk state. Tap name `tapNNNNNNNN` from first 8 hex chars of machine UUID (IFNAMSIZ-safe); MAC `06:00` + IP octets (spike's scheme). Guest IP reported in `Status` and recorded on the machines row (`guest_ip inet`) for the Phase 3 gateway. | The agent owns the host netns, so it owns allocation; persisted allocations prevent double-allocation across restarts. Control plane stores the IP but never computes it. |
| 7 | **Schema**: migration `000002` adds `hosts` (minimal), `machines`, `machine_events` (DDL below). State CHECK includes `starting` (used now) and `hibernating` (reserved for Phase 4). Single host **seeded at control-plane startup from env** (upsert by name). Machine spec + pinned `kernel_ref`/`rootfs_ref` come from config (`PROTEOS_MACHINE_VCPUS=2`, `PROTEOS_MACHINE_MEM_MIB=2048`, `PROTEOS_KERNEL_REF`, `PROTEOS_ROOTFS_REF`), stamped per machine row at create. | Per-row image pinning is what lets Phase 12 upgrade machines between image versions. |
| 8 | **Stop semantics this phase**: graceful (`SendCtrlAltDel`, kill after timeout). Rootfs = fresh writable copy of the pinned base image per boot, discarded on destroy. `start` from `stopped` = cold boot via `EnsureRunning`. | Matches the plan doc ("stop = plain shutdown; hibernate is Phase 4"); the copy-into-chroot step is required by jailer anyway. |

## Schema (`controlplane/migrations/000002_machines.up.sql`)

```sql
CREATE TABLE hosts (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE,
    agent_url   text NOT NULL,
    status      text NOT NULL DEFAULT 'active',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE machines (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE, -- 1:1
    state          text NOT NULL DEFAULT 'requested'
                   CHECK (state IN ('requested','provisioning','running','starting',
                                    'stopping','hibernating','stopped','error')),
    host_id        uuid REFERENCES hosts(id),
    vm_handle      text,
    guest_ip       inet,
    kernel_ref     text NOT NULL,
    rootfs_ref     text NOT NULL,
    resource_spec  jsonb NOT NULL,          -- {"vcpus":2,"mem_mib":2048}
    last_error     text,
    last_active_at timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE machine_events (
    id          bigserial PRIMARY KEY,      -- doubles as SSE Last-Event-ID
    machine_id  uuid NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
    type        text NOT NULL,              -- 'transition' | 'error' | 'info'
    from_state  text,
    to_state    text,
    actor       text NOT NULL,              -- 'user:<uuid>' | 'system:poller' | 'system:api'
    payload     jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_machine_events_machine ON machine_events(machine_id, id);
```

**Allowed transitions** (single source of truth in Go): `requested→provisioning|error`, `provisioning→running|error`, `running→stopping|error` (crash sweep), `stopping→stopped|error`, `stopped→starting`, `starting→running|error`, `error→starting` (retry).

## Wire contracts

### Node-agent API (bearer `Authorization: Bearer $PROTEOS_AGENT_TOKEN`; types in `nodeagent/api`)

```
GET    /healthz                 → 200 {"status":"ok"}            (unauthenticated)
PUT    /v1/machines/{id}        → 202 {"handle":"fc-<id8>"}      idempotent ensure-running
         body: {"vcpus":2,"mem_mib":2048,"kernel_ref":"...","rootfs_ref":"..."}
POST   /v1/machines/{id}/stop   → 202                            graceful shutdown (async)
GET    /v1/machines/{id}        → 200 {"machine_id","state":"creating|running|stopping|stopped|error",
                                       "reason","handle","guest_ip"} | 404 {"error":"unknown_machine"}
GET    /v1/machines             → 200 {"machines":[...]}         (reconciliation)
DELETE /v1/machines/{id}        → 204                            destroy + cleanup
```

Agent states are driver-level; the control plane owns the mapping to machine states.

### Control-plane API

```
GET  /api/machine        → 200 MachineSummary | 404 no_machine
POST /api/machine        → 202 MachineSummary (provisioning) | 409 machine_exists
POST /api/machine/start  → 202 | 409 invalid_state
POST /api/machine/stop   → 202 | 409 invalid_state
GET  /api/machine/events → SSE: `snapshot` on connect (machine + last 50 events),
                           then `machine` events (id: = event row id), ping every 25s

MachineSummary: {id, state, guest_ip|null, kernel_ref, rootfs_ref,
                 resource_spec:{vcpus,mem_mib}, last_error|null, created_at}
```

`GET /api/me` starts returning the real machine summary (replace hardcoded nil in `handlers.go`).

## Package layout

```
go.work                                   # use ./controlplane ./nodeagent
nodeagent/
  go.mod                                  # module github.com/tavon/proteos/nodeagent
  cmd/nodeagent/main.go
  api/                                    # package agentapi — wire types, imported by controlplane
  internal/
    config/                               # PROTEOS_AGENT_ADDR/TOKEN/DATA_DIR, PROTEOS_AGENT_DRIVER=dev|firecracker,
                                          # subnet, images dir + manifest
    httpapi/                              # routes, bearer middleware (constant-time), JSON helpers
    state/                                # per-machine state.json (spec, handle, tap, ip, mac, pid;
                                          # atomic write-rename) + IP allocator
    driver/                               # Driver interface + VMSpec/NetConfig/Status types
    driver/dev/                           # DevDriver (stub child process)
    driver/firecracker/                   # linux-only; integration tests behind `firecracker` build tag
controlplane/internal/
  machine/                                # states, transition table, Service, Transition helper,
                                          # poller.go, broker.go
  nodeclient/                             # HTTP client for agentapi
  httpapi/machine.go                      # real handlers replace Phase 1 stubs
controlplane/migrations/000002_machines.{up,down}.sql
web/src/api/client.ts, hooks.ts, components/MachineCard.tsx
```

## Tasks (Track A = Mac/dev-driver; Track B = Proxmox/Firecracker)

### 2.0 — Run the Firecracker spike (Track B; parallel with 2.1–2.6)
Execute `spike/firecracker/01..07` on the Proxmox VM per `00-proxmox-vm.md`; commit `versions.lock`; record findings (boot timings, jailer quirks, chroot layout, nftables availability, clock/entropy after restore) in the spike README. **Feeds 2.7.**

### 2.1 — Workspace scaffolding + CI (Track A)
`nodeagent/` module with `/healthz`; `nodeagent/api`; root `go.work` (+ `go.work.sum`); controlplane imports `agentapi`. CI: add `nodeagent` job (vet/build/test, no Postgres); verify the existing controlplane job is unaffected by `go.work`.
**Done when:** both modules build in CI from a fresh clone.

### 2.2 — Schema + store + state machine (Track A; after 2.1, parallel with 2.3)
Migration `000002`; sqlc queries (CreateMachine, GetMachineByUserID/ID, guarded `UpdateMachineState … WHERE id=$1 AND state=$2 RETURNING *`, SetMachineRuntime, ListMachinesInStates, InsertMachineEvent, ListMachineEventsAfter/Recent, UpsertHostByName); regen. `internal/machine` transition table + `Transition` (CAS + event insert, same tx) + typed `ErrInvalidTransition`. **Extend `testutil/postgres.go` truncate — `TRUNCATE users CASCADE` won't clear `hosts`.**
**Done when:** migrations up/down clean; illegal transitions provably rejected; every legal transition writes exactly one event row in the same tx.

### 2.3 — node-agent: API, state store, DevDriver (Track A; after 2.1)
Bearer middleware, routes per contract, `internal/state` (atomic JSON, IP allocator), `Driver` interface, DevDriver (boot delay `PROTEOS_DEV_BOOT_DELAY` ~2s; `dev:fail-boot` → error; re-attach by stored pid + liveness probe on startup). Tests: full HTTP API over DevDriver incl. agent restart/re-attach and the failure path.
**Done when:** `go run ./cmd/nodeagent` on a Mac serves the API and "boots" fake machines through creating→running→stopping→stopped.

### 2.4 — Control-plane lifecycle (Track A; after 2.2 + 2.3)
`internal/nodeclient`; config additions (`PROTEOS_HOST_NAME`, `PROTEOS_NODE_AGENT_URL`, `PROTEOS_AGENT_TOKEN`, machine spec, kernel/rootfs refs) in `config.go`; host seeding at startup; `machine.Service` (create: insert `requested` → `provisioning` → agent PUT; start: `stopped|error→starting` → agent PUT; stop: `running→stopping` → agent POST stop); poller (advance transitional states, 30s running sweep, error-with-reason on failure/unreachable/crash); wire real handlers into `server.go`, fill `/api/me`. Tests: e2e against an httptest fake node-agent (Phase 1 fake-GitHub pattern) — happy path, boot failure, agent unreachable, restart-from-stopped, 409s; assert event rows for every transition.
**Done when:** full create→running→stop→stopped→start→running cycle passes in tests and against the real DevDriver-backed agent locally.

### 2.5 — SSE endpoint (Track A; after 2.4)
`machine.Broker`; publish after commit (service + poller); `GET /api/machine/events` handler (flusher, snapshot-then-stream, `Last-Event-ID` replay, 25s heartbeat, cleanup on `r.Context()` done). Register behind `requireAuth` (GET — no CSRF); extend the table-driven authz test.
**Done when:** two transitions appear on a live stream in order with correct `id:` fields; reconnect with `Last-Event-ID` replays the missed event.

### 2.6 — React dashboard (Track A; after 2.4, finalize after 2.5)
`client.ts`: `MachineSummary`, `MachineEvent`, machine endpoints (404→null for GET). `hooks.ts`: `useMachine`, create/start/stop mutations, `useMachineEvents` (EventSource → query-cache writes + event log, reconnect on error). `MachineCard` replaces the disabled empty-state in `Dashboard.tsx`: state badge, state-appropriate buttons, spinners for transitional states, `last_error` banner, event log.
**Done when:** on a Mac with DevDriver, Create shows provisioning→running live without refresh; Stop/Start work; forced `dev:fail-boot` shows error + reason.

### 2.7 — FirecrackerDriver (Track B; after 2.0 + 2.3; final implementation task)
Written against spike findings, mirroring `spike/firecracker/lib.sh` + `06-jailer.sh`: chroot prep (copy pinned kernel + fresh rootfs copy into `<chroot-base>/firecracker/<id>/root`, per-VM uid from a configured range); jailer exec (`--id --exec-file --uid/--gid --chroot-base-dir --cgroup-version 2 --cgroup`); API socket calls `PUT /machine-config`, `/boot-source` (static-IP `ip=` cmdline), `/drives/rootfs`, `/network-interfaces/eth0` **before** `InstanceStart` (no hot-add); tap creation + gateway addressing; **default-deny egress** per tap (nftables: drop guest→host/control-plane/agent/RFC1918/link-local, allow established return, NAT masquerade to internet); graceful stop (`SendCtrlAltDel` → kill after timeout); `Destroy` removes tap/rules/jail/state; startup re-attach (probe jailed socket/pid). Images pinned to `versions.lock`. Integration tests behind `//go:build firecracker`: boot-to-running, stop, status-after-agent-restart, egress denial.
**Done when:** the same node-agent binary with `PROTEOS_AGENT_DRIVER=firecracker` passes the 2.3 API test-suite semantics on the Proxmox VM, every VMM is jailed, and the egress test proves the guest cannot reach the node-agent or control plane but can reach the internet.

### 2.8 — KVM CI job + acceptance pass (Track B; after 2.7)
Self-hosted runner (labels `[self-hosted, linux, kvm]`) on Proxmox; CI job `go test -tags=firecracker ./internal/driver/firecracker/...` with pre-provisioned pinned kernel/rootfs, gated `if: vars.HAS_KVM_RUNNER == 'true'` so CI stays green pre-runner. Then run the full stack on the Proxmox VM and check off every Phase 2 acceptance criterion in `plans/proteos-poc-to-prod.md`.

### Sequencing

```
2.0 (Track B, spike) ───────────────────────────┐
2.1 ──► 2.2 ──┬──► 2.4 ──► 2.5 ──► 2.6          ▼
       └► 2.3 ┘        └────────────────► 2.7 ──► 2.8
```

## Acceptance-criteria mapping (plan-doc Phase 2 checklist)

| Criterion | Task |
|---|---|
| `machines`/`machine_events`/`hosts` tables | 2.2 |
| `POST /api/machine` → provisioning → real FC VM running | 2.4 + 2.7 + 2.8 |
| stop/start transitions persisted | 2.4, 2.7 |
| tap + private IP; `vm_handle`/host recorded | 2.3/2.7 + 2.4 |
| every transition writes an event row | 2.2 (mechanism), 2.4 (all paths) |
| dashboard live state; events stream | 2.5, 2.6 |
| authenticated agent channel | 2.3 + 2.4 |
| jailer from first boot | 2.7 |
| pinned kernel/rootfs on machine record | 2.2 + 2.4 + 2.0 |
| basic default-deny egress | 2.7 |
| driver interface + dev driver; full stack on Mac; KVM CI | 2.3, 2.6, 2.8 |

## Critical existing files to modify

- `controlplane/internal/httpapi/server.go` — replace 3 stub registrations, add SSE route, extend `Server` deps
- `controlplane/internal/httpapi/handlers.go` — real machine summary in `/api/me`
- `controlplane/internal/store/queries.sql` + `controlplane/migrations/` — new queries + migration 000002
- `controlplane/cmd/controlplane/main.go` — wire nodeclient, host seeding, service/poller/broker
- `controlplane/internal/config/config.go` — agent + machine-spec config
- `controlplane/internal/testutil/postgres.go` — truncate `hosts` too
- `.github/workflows/ci.yml` — nodeagent job + gated firecracker job
- `web/src/api/client.ts`, `web/src/api/hooks.ts`, `web/src/routes/Dashboard.tsx`

## Verification

- **Unit/integration (Mac)**: `go test -race ./...` in both modules — transition-table tests, lifecycle e2e vs fake agent, node-agent API over DevDriver (incl. restart/re-attach), SSE stream + replay test, authz table test extended for the SSE route.
- **Manual e2e (Mac)**: run Postgres, `nodeagent` (dev driver), `controlplane`, `web` dev server; Create → provisioning→running live; Stop/Start; force `dev:fail-boot` → error + reason visible.
- **Proxmox**: 2.7 integration tests with `-tags=firecracker`; full-stack walkthrough of the Phase 2 checklist; verify from inside a guest that the control plane/agent/host are unreachable but the internet is.
- **CI**: both module jobs green; `sqlc diff` clean; gated KVM job runs once the runner exists.

## Non-goals / deferred

- Hibernate/snapshot/resume, persistent disks (`hibernating` state + `Disks` field reserved) — Phase 4.
- Terminal, guest agent, gateway, per-machine identity secret (`secret/machines/<id>/identity`) — Phase 3.
- Machine-deletion UX (agent `DELETE` exists for cleanup only; no `/api/machine` DELETE route).
- mTLS/per-host creds, multi-host scheduling, LISTEN/NOTIFY SSE, full reconciliation — Phase 11 (poller and bearer middleware are the designed seams).
- Per-tenant egress rate limits / full egress policy — Phase 10 (only basic default-deny ships now).
- Kernel/rootfs build automation — Phase 12 (pin prebuilt artifacts per `versions.lock`).
