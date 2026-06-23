-- Phase 3 (file-kind profile items): a file item needs a permission mode in
-- addition to its $HOME-relative path (stored in `target`). mode is the octal
-- permission as an integer (e.g. 384 = 0600); NULL for env-kind items, which
-- have no file mode. The value itself stays in OpenBao — only this non-secret
-- metadata lives here.
ALTER TABLE profile_items ADD COLUMN mode integer;
