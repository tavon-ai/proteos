-- Phase 5 schema: the AI-provider registry and the audit log.
--
-- providers is the source of truth for which agent CLIs exist, the command that
-- launches each, and which env vars map to which fields of the user's provider
-- secret (in OpenBao). It is DB-backed from day one so Phase 6 (Gemini/Codex/…)
-- is data + rootfs additions, not schema work. The image/template ref column the
-- master plan mentions is deferred to Phase 6 — in the microVM architecture CLIs
-- are baked into the rootfs, so Phase 5 needs only the launch command.
CREATE TABLE providers (
    key            text PRIMARY KEY,
    display_name   text NOT NULL,
    launch_command text NOT NULL,
    secret_env     jsonb NOT NULL DEFAULT '{}'::jsonb,
    enabled        boolean NOT NULL DEFAULT true,
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- The one seeded row keeps the registry honest: it is read by real code paths,
-- not hardcoded. secret_env maps env var → field in the user's provider secret.
INSERT INTO providers (key, display_name, launch_command, secret_env, enabled)
VALUES ('claude', 'Claude Code', 'claude', '{"ANTHROPIC_API_KEY":"api_key"}'::jsonb, true);

-- audit_log is the early slice of the Phase 10 audit table (decision #6): rows
-- are written on provider key put/delete, injection reads (target = path, never
-- the value), and agent launches. user_id is nullable because system actors
-- (the injector) have no user. No FK to users so audit rows survive user
-- deletion (audit must outlive its subjects).
CREATE TABLE audit_log (
    id        bigserial PRIMARY KEY,
    ts        timestamptz NOT NULL DEFAULT now(),
    user_id   uuid,
    actor     text NOT NULL,
    action    text NOT NULL,
    target    text NOT NULL DEFAULT '',
    metadata  jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX idx_audit_log_ts ON audit_log(ts);
CREATE INDEX idx_audit_log_user_id ON audit_log(user_id);
