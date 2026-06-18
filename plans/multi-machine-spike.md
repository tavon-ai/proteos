# Multiple machines per user

## Context

Today ProteOS enforces a strict 1:1 user↔machine model: a `UNIQUE` constraint on
`machines.user_id`, a `GetMachineByUserID` lookup, and a single-machine API/UI.
Now that create/start/stop/destroy all work, the next step is letting a user run
several machines at once (e.g. one per project/experiment).

Good news from the codebase survey: most per-machine plumbing already exists and
needs **no change** — terminals (`Terminal` takes a `machineID`), the editor /
web-session, machine-web subdomain routing (`m-<uuid>`), the secret injector, and
the poller are all machine-id-scoped, and the gateway already has an
ownership-checked resolver (`resolveTerminalMachine`, `controlplane/internal/httpapi/gateway.go:76`).
The work is concentrated in: the DB constraint, the service lifecycle methods, the
lifecycle HTTP routes, the SSE stream, and the frontend (which needs a machine
switcher).

### Product decisions (confirmed)
- **UI model:** active machine + switcher. One machine's desktop at a time; a
  switcher in the taskbar selects the active machine; its windows/layout are
  scoped to it and saved per-machine.
- **Naming:** auto-named (`machine-1`, `machine-2`, …) on create; rename later.
- **Cap:** configurable per-user max, default 3 (`PROTEOS_MAX_MACHINES_PER_USER`);
  create returns `409 machine_limit` when exceeded.

---

## Part 1 — Backend (control plane)

### 1.1 DB migration — `controlplane/migrations/000006_multi_machine.{up,down}.sql`
- Drop the 1:1 constraint: `ALTER TABLE machines DROP CONSTRAINT machines_user_id_key;`
  (Postgres auto-named the inline `UNIQUE` from `000002_machines.up.sql:16`.)
- Add `name`: `ALTER TABLE machines ADD COLUMN name text NOT NULL DEFAULT '';`
- Add a lookup index now that `user_id` is non-unique:
  `CREATE INDEX idx_machines_user ON machines(user_id);`
- Down: drop index + column, restore the unique constraint.

### 1.2 Store queries — `controlplane/internal/store/queries.sql` (then `sqlc generate`)
- **Add** `ListMachinesByUserID :many` (`WHERE user_id=$1 ORDER BY created_at`).
- **Add** `CountMachinesByUserID :one` (cap check).
- **Add** `RenameMachine :one` (`UPDATE machines SET name=$2 … RETURNING *`).
- **Change** `CreateMachine` to accept `name`.
- Keep `GetMachineByID` (already used). Keep `GetMachineByUserID` only for the
  single-machine fallback (see 1.4); everything else moves to id-based.

### 1.3 Service — `controlplane/internal/machine/service.go`
- Add `Spec.MaxPerUser int` (wired from env; 0 ⇒ default 3) and a
  `ErrMachineLimit` sentinel.
- `Create(ctx, userID, name)`: **remove** the `ErrMachineExists` 1:1 check
  (service.go:116-120); instead `CountMachinesByUserID` ≥ cap ⇒ `ErrMachineLimit`.
  Auto-name when `name==""` → `machine-<count+1>`. Rest of the flow (disk + volume
  key + transition + `ensureOnAgent`) is unchanged.
- `Start/Stop/Destroy(ctx, userID, machineID)`: resolve via a new private
  `getOwned(ctx, userID, machineID)` (GetByID + `uuidEqual`, else `ErrNoMachine`)
  rather than `Get(userID)`. Logic otherwise identical.
- Add `List(ctx, userID) ([]store.Machine, error)` and
  `Rename(ctx, userID, machineID, name)`.
- `ErrMachineExists` becomes unused — remove it and its 409 mapping.

### 1.4 HTTP API — `controlplane/internal/httpapi/`
RESTful machine collection (replaces the singular `/api/machine*` routes in
`server.go:95-99`):
- `GET    /api/machines`            → list (`[]MachineSummary`)
- `POST   /api/machines`            → create (auto-named); `201`/`202` + summary;
  `409 machine_limit` when capped
- `GET    /api/machines/{id}`       → one (ownership)
- `POST   /api/machines/{id}/start`
- `POST   /api/machines/{id}/stop`
- `DELETE /api/machines/{id}`
- `PATCH  /api/machines/{id}`       → rename `{ "name": "…" }`

Implementation notes:
- Generalize `machineMutation` (machine.go:159) to take the `{id}` path value,
  resolve+own via `getOwned`, map `ErrNoMachine→404`, `ErrInvalidState→409`,
  `ErrMachineLimit→409`.
- Add `Name string` to `MachineSummary` + `toSummary` (machine.go:17,43).
- **`/api/me`** (handlers.go): `Machine *MachineSummary` → `Machines []MachineSummary`
  (seeds first paint; `handleMe` calls `Machines.List`).
- **Per-machine handlers** that today resolve the user's single machine — git
  clone (git.go:117), projects (projects.go:41), desktop get/put
  (desktop.go:`runningMachineID`) — switch to the existing
  `resolveTerminalMachine(ctx, user, r.URL.Query().Get("machine"))` so they accept
  `?machine=<id>`. The frontend always passes the active machine id.
- **`resolveTerminalMachine` empty-param fallback** (gateway.go:77): change from
  "the user's machine" to "if the user has exactly one machine use it, else
  `ErrNoMachine`" so an arbitrary-row pick is impossible. Add a
  `Machines.OnlyMachine(userID)` helper or inline via `List`.

### 1.5 SSE — `controlplane/internal/httpapi/sse.go`
Make the stream user-scoped (all the user's machines) instead of single-machine:
- **snapshot**: list the user's machines; emit each machine summary + recent
  events. New `snapshotData` shape: `{ machines: [...], events: [...] }`.
- **live**: the broker already fans out all updates and the loop already filters
  `sameUser` (sse.go:124) — a `machine` event carries the full summary (frontend
  upserts by id); the `destroyed` event already carries `machine_id`. No per-machine
  filter needed.
- **replay** (Last-Event-ID): replay events after `lastID` across the user's
  machine set (ids are a global `bigserial`, so order is preserved). Add
  `ListMachineEventsAfterForUser` (join machines on user_id) or loop the user's
  machines and merge by id.

### 1.6 Config — `controlplane/internal/config` + `cmd/controlplane`
Read `PROTEOS_MAX_MACHINES_PER_USER` (default 3) into `machine.Spec.MaxPerUser`.

---

## Part 2 — Frontend (`web/src/`)

### 2.1 API client — `api/client.ts`
- `Me.machine` → `Me.machines: MachineSummary[]`; add `name: string` to
  `MachineSummary`.
- Lifecycle by id: `listMachines()`, `createMachine()`, `startMachine(id)`,
  `stopMachine(id)`, `destroyMachine(id)`, `renameMachine(id, name)`.
- Add a `machine` query param to `webSession`, `getDesktop`, `putDesktop`,
  `listProjects`, `cloneRepo` (pass the active machine id).

### 2.2 Hooks + selected-machine state — `api/hooks.ts` (+ small context)
- `machineKey ['machine']` → `machinesKey ['machines']`; `useMachines(initial)`
  returns the list.
- `useMachineMutations`: mutations take an `id`; create invalidates the list;
  destroy removes by id from the cached list.
- `useMachineEvents`: on `machine` event upsert the summary into the list by id;
  on `destroyed` remove by id; snapshot seeds the whole list.
- **Selected machine**: a small `useSelectedMachine` (localStorage-persisted id +
  context). Default = first `running`, else first, else none. Exposes
  `selectedId`, `setSelectedId`, and the resolved `MachineSummary | null`.

### 2.3 Taskbar switcher — `desktop/Taskbar.tsx`
- Replace the single state badge with a **machine switcher dropdown**: lists each
  machine (`name · state`), selects the active one, has “+ New machine” and inline
  rename. Start/Stop/Destroy operate on the **selected** machine id. “Create”
  always available (until cap), shows `machine_limit` as a disabled/tooltip state.

### 2.4 Desktop scoping + per-machine layout — `desktop/Desktop.tsx`, `desktop/useLayout.ts`
- Desktop binds to the **selected** machine (passes its id to `Terminal`/editor —
  already props). Terminals/editor unchanged otherwise.
- `useLayout`: scope load/save to the selected machine id (pass `?machine=`); on
  machine switch, save the outgoing machine's layout, clear windows, load the
  incoming machine's layout (layout lives in that machine's SQLite — only
  available while running; a stopped machine opens an empty desktop, same as today).

---

## Part 3 — Tests & verification

### Automated
- **Go**: extend `internal/machine/lifecycle_test.go` for multi-machine
  (create N, cap → `ErrMachineLimit`, start/stop/destroy by id, ownership reject,
  rename); update `authz_test.go` to the new `/api/machines*` routes; update
  `stubNodeClient`/fixtures only if signatures change. SSE test for multi-machine
  snapshot/replay.
- **Web**: `npm run typecheck && lint && knip && vitest run`. Extend
  `windowState.test.ts` if layout-per-machine logic moves there.

### Manual (real app, the way the user just validated destroy)
1. Create machine → it appears in the switcher (auto-named `machine-1`), boots.
2. Create a 2nd and 3rd; 4th → `409 machine_limit` surfaced in the UI.
3. Switch between machines: each shows its own desktop/terminals/layout; open a
   home Terminal on each and confirm it attaches to the right guest.
4. Rename a machine; reload → name persists; selection persists (localStorage).
5. Clone a repo into the **selected** machine; confirm it lands there and not the
   others. Stop/Start/Destroy a non-selected machine via the switcher; SSE updates
   its badge live without touching the others.

---

## Suggested landing order
1. **Backend** (Part 1) — migration → store → service → routes → `/api/me` → SSE →
   config. Shippable on its own (API supports N machines; frontend still reads
   `machines[0]`).
2. **Frontend** (Part 2) — list cache + selected-machine + switcher + desktop
   scoping + per-machine layout.
3. **Tests/polish** (Part 3).

## Notes / non-goals
- Node-agent needs **no change** — it is already keyed by machine id and allocates
  a guest IP per machine from the host subnet (the cap protects that pool).
- Git tokens stay **per-user** (`/api/git/repos` unchanged); only the clone
  *target* becomes machine-scoped.
- Not doing simultaneous cross-machine windows (the "Machines manager" model) —
  deferred per the chosen active-machine+switcher UX.
