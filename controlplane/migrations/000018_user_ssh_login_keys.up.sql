-- Inbound SSH login keys (distinct from the outbound git-SSH identity in
-- profile_items — see controlplane/internal/profile/sshkey.go). A user may
-- register any number of public keys; every active (non-revoked) key is
-- injected into every machine they own as ~/.ssh/authorized_keys.
--
-- Public keys are not secret, so they live directly in Postgres (no OpenBao
-- round-trip). revoked_at is a soft delete so revocation history survives; the
-- partial unique index only guards against re-adding the same active key twice
-- (a revoked key's fingerprint may be re-added later).
CREATE TABLE user_ssh_login_keys (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    label       text NOT NULL,
    public_key  text NOT NULL,
    fingerprint text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz
);

CREATE UNIQUE INDEX user_ssh_login_keys_user_fingerprint_key
    ON user_ssh_login_keys (user_id, fingerprint)
    WHERE revoked_at IS NULL;

CREATE INDEX user_ssh_login_keys_user_id_idx ON user_ssh_login_keys (user_id);
