-- Portable user profile (Phase 1): per-user profile items that materialize into
-- every one of the user's machines at injection time, so credentials/dotfiles
-- follow the *user*, not the per-machine LUKS volume.
--
-- This table holds ONLY presence/metadata. The secret value lives exclusively in
-- OpenBao at secret/users/<id>/profile/<key> (covered by the existing user-<id>
-- policy, a sibling of the providers/ namespace). The metadata here drives UI
-- status and lets the injector know what to fetch without listing OpenBao.
--
-- kind is generic from day one: 'env' (Tier 0 — target is an env var name) and,
-- later, 'file' (Phase 3 — target is a $HOME-relative path/mode). target carries
-- the kind-specific destination; the server-side Def registry is authoritative
-- for an item's kind/target, so a client only ever supplies the value.
CREATE TABLE profile_items (
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key        text NOT NULL,                       -- item key, e.g. 'claude-oauth'
    kind       text NOT NULL,                       -- 'env' (Phase 1) | 'file' (Phase 3+)
    target     text NOT NULL DEFAULT '',            -- env: variable name; file: path/mode
    expires_at timestamptz,                          -- optional; e.g. Claude token ~1y out
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key)
);
