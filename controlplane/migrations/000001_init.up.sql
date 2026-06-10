-- Phase 1 schema: identity, sessions, and the GitHub link (token-free).
-- Token material never lands here; github_links stores only a secret_ref that
-- points into the secrets store (dev filestore now, OpenBao in Phase 5).

CREATE TABLE users (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    github_user_id  bigint      NOT NULL UNIQUE,
    login           text        NOT NULL,
    email           text        NOT NULL DEFAULT '',
    avatar_url      text        NOT NULL DEFAULT '',
    status          text        NOT NULL DEFAULT 'active',
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash  bytea       NOT NULL UNIQUE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    revoked_at  timestamptz
);

CREATE INDEX idx_sessions_user_id ON sessions(user_id);

CREATE TABLE github_links (
    user_id      uuid        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    metadata     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    secret_ref   text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
