-- The user's project-download preference. false (default) ⇒ a "clean" export
-- (working tree minus .git and .gitignore'd files); true ⇒ the full directory
-- tree exactly as it is on disk. Surfaced in the Settings page and consumed by
-- the /api/projects/download proxy.
ALTER TABLE users ADD COLUMN download_as_is boolean NOT NULL DEFAULT false;
