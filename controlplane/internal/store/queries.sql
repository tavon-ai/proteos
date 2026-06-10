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

-- name: TouchSession :exec
-- Slide the expiry forward on use (sliding-refresh sessions).
UPDATE sessions SET expires_at = $2 WHERE id = $1;

-- name: RevokeSession :exec
UPDATE sessions SET revoked_at = now()
WHERE token_hash = $1 AND revoked_at IS NULL;

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
-- Create a user's (1:1) machine in the initial 'requested' state, pinning the
-- image refs and resource spec for the lifetime of the row.
INSERT INTO machines (user_id, host_id, state, kernel_ref, rootfs_ref, resource_spec)
VALUES ($1, $2, 'requested', $3, $4, $5)
RETURNING *;

-- name: GetMachineByUserID :one
SELECT * FROM machines WHERE user_id = $1;

-- name: GetMachineByID :one
SELECT * FROM machines WHERE id = $1;

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
