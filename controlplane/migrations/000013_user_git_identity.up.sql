-- Phase 4: a user's portable git identity (name + email) for the portable
-- profile. It is NON-secret, so it lives in Postgres (not OpenBao) and is read by
-- the existing Phase 7 git.configure control op, which is the single writer of
-- ~/.gitconfig — the profile identity simply overrides the GitHub-derived default
-- there, so the two never fight over the file (the reconciliation Phase 4 calls
-- for). Absent row ⇒ fall back to the GitHub identity.
CREATE TABLE user_git_identity (
    user_id    uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    name       text NOT NULL,
    email      text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
