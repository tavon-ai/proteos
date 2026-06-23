-- name: UpsertUser :one
-- Insert a user keyed by their GitHub user id, updating profile fields on
-- repeat logins. Returns the full row (id is stable across logins).
INSERT INTO users (github_user_id, login, email, avatar_url)
VALUES ($1, $2, $3, $4)
ON CONFLICT (github_user_id) DO UPDATE
    SET login = EXCLUDED.login,
        email = EXCLUDED.email,
        avatar_url = EXCLUDED.avatar_url
RETURNING *;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: SetUserDownloadAsIs :one
-- Update the user's project-download preference (true ⇒ download the full tree
-- as-is including .git and ignored files; false ⇒ the clean export). Returns the
-- updated row.
UPDATE users SET download_as_is = $2 WHERE id = $1 RETURNING *;

-- name: CreateSession :one
INSERT INTO sessions (user_id, token_hash, expires_at)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetSessionByTokenHash :one
-- Look up a live (unexpired, unrevoked) session by its token hash, returning
-- the session together with the owning user.
SELECT
    sqlc.embed(sessions),
    sqlc.embed(users)
FROM sessions
JOIN users ON users.id = sessions.user_id
WHERE sessions.token_hash = $1
  AND sessions.revoked_at IS NULL
  AND sessions.expires_at > now();

-- name: GetSessionByID :one
-- Look up a live (unexpired, unrevoked) session by its id, returning the session
-- with the owning user. Used by the Phase 8 machine-web cookie path, which binds
-- the subdomain cookie to the parent session id (never the session token), so a
-- logout/revoke of the parent immediately invalidates the editor.
SELECT
    sqlc.embed(sessions),
    sqlc.embed(users)
FROM sessions
JOIN users ON users.id = sessions.user_id
WHERE sessions.id = $1
  AND sessions.revoked_at IS NULL
  AND sessions.expires_at > now();

-- name: TouchSession :exec
-- Slide the expiry forward on use (sliding-refresh sessions).
UPDATE sessions SET expires_at = $2 WHERE id = $1;

-- name: RevokeSession :one
-- Revoke a live session by token hash, returning its id so the gateway can
-- close any in-process WebSockets bound to it. No row (ErrNoRows) means the
-- token was unknown or already revoked — a no-op.
UPDATE sessions SET revoked_at = now()
WHERE token_hash = $1 AND revoked_at IS NULL
RETURNING id;

-- name: UpsertGitHubLink :one
INSERT INTO github_links (user_id, metadata, secret_ref, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (user_id) DO UPDATE
    SET metadata = EXCLUDED.metadata,
        secret_ref = EXCLUDED.secret_ref,
        updated_at = now()
RETURNING *;

-- name: GetGitHubLink :one
SELECT * FROM github_links WHERE user_id = $1;

-- name: UpsertHostByName :one
-- Seed/refresh a host by its unique name at control-plane startup.
INSERT INTO hosts (name, agent_url)
VALUES ($1, $2)
ON CONFLICT (name) DO UPDATE
    SET agent_url = EXCLUDED.agent_url
RETURNING *;

-- name: CreateMachine :one
-- Create one of a user's machines in the initial 'requested' state, pinning the
-- image refs and resource spec for the lifetime of the row. name is the display
-- label (auto-named machine-N by the service; renameable later). template_id is
-- the catalog template the machine was created from (NULL for legacy machines).
INSERT INTO machines (user_id, host_id, state, name, kernel_ref, rootfs_ref, resource_spec, template_id)
VALUES ($1, $2, 'requested', $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetMachineByUserID :one
-- The user's single machine. Retained only for the single-machine fallback in
-- the gateway resolver; multi-machine callers use ListMachinesByUserID + an id.
SELECT * FROM machines WHERE user_id = $1;

-- name: ListMachinesByUserID :many
-- All of a user's machines, oldest-first (stable order for the switcher).
SELECT * FROM machines WHERE user_id = $1 ORDER BY created_at ASC, id ASC;

-- name: CountMachinesByUserID :one
-- Number of machines a user owns, for enforcing the per-user cap on create.
SELECT count(*) FROM machines WHERE user_id = $1;

-- name: ListRunningMachineIDsByUserID :many
-- The ids of a user's currently-running machines. Drives re-injection of a
-- profile/secret change to already-running machines (Phase 2 portable profile),
-- so a change takes effect without recreating a machine.
SELECT id FROM machines WHERE user_id = $1 AND state = 'running' ORDER BY id;

-- name: RenameMachine :one
-- Set a machine's display name. Ownership is enforced by the caller.
UPDATE machines SET name = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: GetMachineByID :one
SELECT * FROM machines WHERE id = $1;

-- name: DeleteMachine :exec
-- Hard-delete a machine row. Cascades to its disk, snapshot, and machine_events
-- (all ON DELETE CASCADE). The node-agent VM teardown and the secret-store
-- volume-key deletion are done by the service before this runs.
DELETE FROM machines WHERE id = $1;

-- name: UpdateMachineState :one
-- Guarded compare-and-set transition: only updates if the row is still in the
-- expected from-state, so illegal/raced transitions affect zero rows (the Go
-- layer turns that into ErrInvalidTransition). last_error is set on the same
-- write (pass NULL to clear it on a successful forward transition).
UPDATE machines
SET state = @to_state,
    last_error = @last_error,
    updated_at = now()
WHERE id = @id AND state = @from_state
RETURNING *;

-- name: SetMachineRuntime :one
-- Record runtime facts reported by the node-agent (handle + allocated guest IP)
-- once a machine is provisioned.
UPDATE machines
SET vm_handle = $2,
    guest_ip = $3,
    last_active_at = now(),
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ListMachinesInStates :many
-- Used by the poller to find transitional machines to advance and running
-- machines to sweep.
SELECT * FROM machines WHERE state = ANY($1::text[]) ORDER BY updated_at ASC;

-- name: InsertMachineEvent :one
-- Append one audit row. Always called in the same tx as the state change it
-- records (see internal/machine.Transition).
INSERT INTO machine_events (machine_id, type, from_state, to_state, actor, payload)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListMachineEventsRecent :many
-- Most-recent N events for a machine, returned oldest-first for the SSE
-- snapshot.
SELECT * FROM (
    SELECT * FROM machine_events
    WHERE machine_id = $1
    ORDER BY id DESC
    LIMIT $2
) sub
ORDER BY id ASC;

-- name: ListMachineEventsAfter :many
-- Events for a machine after a given event id, for SSE Last-Event-ID replay.
SELECT * FROM machine_events
WHERE machine_id = $1 AND id > $2
ORDER BY id ASC;

-- name: ListUserMachineEventsRecent :many
-- Most-recent N events across ALL of a user's machines, oldest-first, for the
-- multi-machine SSE snapshot.
SELECT sub.* FROM (
    SELECT me.* FROM machine_events me
    JOIN machines m ON m.id = me.machine_id
    WHERE m.user_id = $1
    ORDER BY me.id DESC
    LIMIT $2
) sub
ORDER BY sub.id ASC;

-- name: ListUserMachineEventsAfter :many
-- Events across ALL of a user's machines after a given id, oldest-first, for SSE
-- Last-Event-ID replay (ids are a global sequence so cross-machine order holds).
SELECT me.* FROM machine_events me
JOIN machines m ON m.id = me.machine_id
WHERE m.user_id = $1 AND me.id > $2
ORDER BY me.id ASC;

-- name: CreateDisk :one
-- Provision a machine's persistent disk at create time (1:1 with the machine).
INSERT INTO disks (machine_id, size_mib)
VALUES ($1, $2)
RETURNING *;

-- name: GetDiskByMachineID :one
SELECT * FROM disks WHERE machine_id = $1;

-- name: SetMachineDisk :one
-- Attach the just-created disk to the machine row.
UPDATE machines SET disk_id = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: SetMachineBoot :one
-- Record how the current run started ('cold' | 'resumed'); pass NULL to clear.
UPDATE machines SET boot = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UpsertSnapshot :one
-- Record (replacing) the machine's current hibernation snapshot metadata. One
-- row per machine; consumed on resume / invalidated on cold stop via DeleteSnapshot.
INSERT INTO snapshots (machine_id, fc_version, mem_bytes, kernel_ref, rootfs_ref)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (machine_id) DO UPDATE
    SET fc_version = EXCLUDED.fc_version,
        mem_bytes  = EXCLUDED.mem_bytes,
        kernel_ref = EXCLUDED.kernel_ref,
        rootfs_ref = EXCLUDED.rootfs_ref,
        created_at = now()
RETURNING *;

-- name: GetSnapshot :one
SELECT * FROM snapshots WHERE machine_id = $1;

-- name: DeleteSnapshot :exec
DELETE FROM snapshots WHERE machine_id = $1;

-- name: ListProviders :many
-- The provider registry, ordered for stable API output.
SELECT * FROM providers ORDER BY key;

-- name: GetProvider :one
SELECT * FROM providers WHERE key = $1;

-- name: SetProvidersEnabled :exec
-- Reconcile the enabled flag (Phase 6): enable exactly the given provider keys
-- and disable every other registered provider, so the registry matches the CLIs
-- actually baked into the rootfs.
UPDATE providers SET enabled = (key = ANY(@keys::text[]));

-- name: InsertAuditLog :one
-- Append one audit row. user_id may be NULL for system actors (the injector).
INSERT INTO audit_log (user_id, actor, action, target, metadata)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: InsertAgentTask :one
-- Create a queued headless agent task (AT1). status defaults to 'queued'.
INSERT INTO agent_tasks (machine_id, user_id, provider, project, prompt)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetAgentTask :one
SELECT * FROM agent_tasks WHERE id = $1;

-- name: ListAgentTasksByMachine :many
-- A machine's tasks, newest first.
SELECT * FROM agent_tasks WHERE machine_id = $1 ORDER BY created_at DESC;

-- name: MarkAgentTaskRunning :exec
-- Move a queued task to running once the run is dispatched.
UPDATE agent_tasks
SET status = 'running', started_at = now()
WHERE id = $1 AND status = 'queued';

-- name: FinishAgentTask :exec
-- Record a terminal outcome (done|failed|canceled) with its result fields. An
-- empty incoming session id preserves the one already captured, so a cancel or a
-- mid-run failure never wipes a session a prior turn established (AT4 resume).
UPDATE agent_tasks
SET status = sqlc.arg(status),
    agent_session_id = COALESCE(NULLIF(sqlc.arg(agent_session_id)::text, ''), agent_session_id),
    usage = sqlc.arg(usage),
    result_summary = sqlc.arg(result_summary),
    error = sqlc.arg(error),
    ended_at = now()
WHERE id = sqlc.arg(id);

-- name: RestartAgentTask :exec
-- Begin a follow-up turn on a finished task (AT4): back to running, store the new
-- turn's prompt, and clear the prior turn's error/end marker. The captured
-- agent_session_id (the resume key) is left intact.
UPDATE agent_tasks
SET status = 'running',
    prompt = $2,
    error = '',
    ended_at = NULL
WHERE id = $1;

-- name: CreatePAT :one
-- Mint a personal access token (AC1). expires_at NULL means it never expires.
INSERT INTO personal_access_tokens (user_id, name, token_hash, prefix, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetPATByTokenHash :one
-- Look up a live (unexpired, unrevoked) token by its hash, with the owning user.
SELECT
    sqlc.embed(personal_access_tokens),
    sqlc.embed(users)
FROM personal_access_tokens
JOIN users ON users.id = personal_access_tokens.user_id
WHERE personal_access_tokens.token_hash = $1
  AND personal_access_tokens.revoked_at IS NULL
  AND (personal_access_tokens.expires_at IS NULL OR personal_access_tokens.expires_at > now());

-- name: ListPATsByUserID :many
-- A user's tokens, newest first. Never selects token_hash for display callers, but
-- the row carries it; the API layer projects only non-secret fields.
SELECT * FROM personal_access_tokens
WHERE user_id = $1 AND revoked_at IS NULL
ORDER BY created_at DESC;

-- name: TouchPATLastUsed :exec
-- Best-effort last-used bump on authentication (throttled by the caller).
UPDATE personal_access_tokens SET last_used_at = now() WHERE id = $1;

-- name: RevokePAT :one
-- Revoke a live token owned by the user, returning its id. No row (ErrNoRows)
-- means it was unknown, not the caller's, or already revoked — a no-op/404.
UPDATE personal_access_tokens SET revoked_at = now()
WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL
RETURNING id;

-- name: UpsertProfileItem :one
-- Set/replace a portable-profile item's metadata (the value lives only in
-- OpenBao). kind/target/mode come from the server-side Def (or, for a generic
-- file item, the request); mode is NULL for env-kind items. updated_at is bumped
-- on every write so the UI can show "last updated".
INSERT INTO profile_items (user_id, key, kind, target, expires_at, mode)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id, key) DO UPDATE
    SET kind = EXCLUDED.kind,
        target = EXCLUDED.target,
        expires_at = EXCLUDED.expires_at,
        mode = EXCLUDED.mode,
        updated_at = now()
RETURNING *;

-- name: ListProfileItems :many
-- A user's profile items (metadata only — never the secret value), keyed order.
SELECT * FROM profile_items WHERE user_id = $1 ORDER BY key;

-- name: UpsertGitIdentity :one
-- Set/replace a user's portable git identity (name + email). Read by the
-- git.configure control op to override the GitHub-derived default.
INSERT INTO user_git_identity (user_id, name, email)
VALUES ($1, $2, $3)
ON CONFLICT (user_id) DO UPDATE
    SET name = EXCLUDED.name, email = EXCLUDED.email, updated_at = now()
RETURNING *;

-- name: GetGitIdentity :one
-- A user's portable git identity, or ErrNoRows when unset (fall back to GitHub).
SELECT name, email FROM user_git_identity WHERE user_id = $1;

-- name: DeleteGitIdentity :execrows
-- Clear a user's portable git identity (revert to the GitHub default). Returns
-- the number of rows removed (0 ⇒ none was set), which the API maps to 404.
DELETE FROM user_git_identity WHERE user_id = $1;

-- name: DeleteProfileItem :execrows
-- Remove a profile item's metadata row, returning the number of rows deleted (0
-- ⇒ the user had no such item, which the API maps to 404). Deleting a missing
-- item is otherwise a harmless no-op.
DELETE FROM profile_items WHERE user_id = $1 AND key = $2;
