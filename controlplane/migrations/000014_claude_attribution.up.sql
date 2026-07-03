-- Whether Claude Code stamps its attribution (the "Generated with Claude Code"
-- commit line, the Co-Authored-By trailer, and the PR-body attribution) on the
-- user's commits and PRs. true (default) leaves Claude Code's own defaults
-- untouched; false blanks them via a merge into ~/.claude/settings.json on the
-- user's machines (claude.configure). Surfaced in the Settings page — some
-- organizations disallow co-authored commits.
ALTER TABLE users ADD COLUMN claude_attribution boolean NOT NULL DEFAULT true;
