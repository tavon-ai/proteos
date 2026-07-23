-- Rolling back to GitHub-only identity: users that exist only through Zitadel
-- have no GitHub identity to fall back to, so they (and their cascaded
-- resources) are removed before github_user_id becomes NOT NULL again.
DELETE FROM users WHERE github_user_id IS NULL;
DROP INDEX users_oidc_identity_key;
ALTER TABLE users
    DROP COLUMN oidc_issuer,
    DROP COLUMN oidc_subject;
ALTER TABLE users ALTER COLUMN github_user_id SET NOT NULL;
