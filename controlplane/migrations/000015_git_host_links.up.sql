-- Gitea/Forgejo phase 2: a user's per-host git credential link. host is the
-- lowercased host[:port] form the PROTEOS_GIT_PUBLIC_HOSTS allowlist and
-- parseRemote both produce. Token material never lands here (same rule as
-- github_links): metadata holds only non-sensitive hints (the host login, for
-- display), and secret_ref points into the secrets store where the PAT lives.
-- Named git_host_links because user_git_identity (000013) is the unrelated
-- commit name/email identity.
CREATE TABLE git_host_links (
    user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    host       text        NOT NULL,
    metadata   jsonb       NOT NULL DEFAULT '{}'::jsonb,
    secret_ref text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, host)
);
