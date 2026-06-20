-- AC1: Personal Access Tokens. The browser session cookie is the only credential
-- the control plane accepts today; a CLI / programmatic caller cannot use it. A
-- PAT is a long-lived, user-minted bearer credential carried in the Authorization
-- header. Like the session token, only its SHA-256 hash is stored, so a database
-- leak does not expose live tokens. `prefix` is a non-secret identifier (the first
-- few characters of the plaintext) shown in listings so a user can tell their
-- tokens apart without ever seeing the secret again.
CREATE TABLE personal_access_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         text NOT NULL DEFAULT '',   -- user label, e.g. "laptop cli"
    token_hash   bytea NOT NULL UNIQUE,       -- SHA-256 of the plaintext token
    prefix       text NOT NULL DEFAULT '',    -- non-secret display identifier
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz,                 -- NULL = never expires
    last_used_at timestamptz,                 -- best-effort, bumped on use
    revoked_at   timestamptz
);

CREATE INDEX idx_pat_user ON personal_access_tokens(user_id, created_at DESC);
