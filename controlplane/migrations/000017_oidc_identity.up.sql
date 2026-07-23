-- TAV-149: login moves to Zitadel (OIDC). A user's identity is now the OIDC
-- (issuer, subject) pair; github_user_id becomes the optional "Connect GitHub"
-- link (required before the app is usable, but not at signup). Existing rows
-- keep github_user_id and gain their OIDC identity on first Zitadel login via
-- verified-email linking.
ALTER TABLE users ALTER COLUMN github_user_id DROP NOT NULL;
ALTER TABLE users
    ADD COLUMN oidc_issuer  text,
    ADD COLUMN oidc_subject text;

-- Partial unique index: legacy rows (NULL identity) don't collide; a given
-- issuer+subject maps to exactly one user.
CREATE UNIQUE INDEX users_oidc_identity_key
    ON users (oidc_issuer, oidc_subject)
    WHERE oidc_subject IS NOT NULL;
