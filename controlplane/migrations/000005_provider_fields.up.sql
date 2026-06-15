-- Phase 6 schema: generalize the provider registry so one shape covers every
-- observed auth style (pure env, login-step, borrowed model key) and the
-- settings UI can be rendered from data (Phase 6 decision #1).
--
-- Phase 5's secret_env (env var → secret field) assumed a single API key whose
-- field name was implicit. Phase 6 replaces it with secret_fields: an ORDERED
-- list of {name,label,env} objects. name is the field stored under the user's
-- provider secret, label is the human prompt the settings UI renders, env is the
-- environment variable the injector composes from that field's value. Field
-- names/labels/env vars are not secret — only values are.
--
-- setup_command is an optional shell command the guest runs once per push to
-- complete login-style auth (e.g. Codex's `codex login --with-api-key`). NULL
-- for providers whose auth is pure-env.
ALTER TABLE providers ADD COLUMN secret_fields jsonb NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE providers ADD COLUMN setup_command text;

-- Restate Claude Code's Phase 5 row in the new shape (behaviour unchanged): the
-- single api_key field maps to ANTHROPIC_API_KEY, no setup step.
UPDATE providers
SET secret_fields = '[{"name":"api_key","label":"Anthropic API key","env":"ANTHROPIC_API_KEY"}]'::jsonb
WHERE key = 'claude';

-- secret_env is fully replaced by secret_fields.
ALTER TABLE providers DROP COLUMN secret_env;

-- Seed the three Phase 6 providers. The CLIs are baked into the rootfs; the
-- registry needs only the launch command, the secret-field metadata, and (for
-- Codex) the idempotent setup command.
INSERT INTO providers (key, display_name, launch_command, setup_command, secret_fields, enabled) VALUES
  ('gemini', 'Gemini CLI', 'gemini', NULL,
   '[{"name":"api_key","label":"Gemini API key","env":"GEMINI_API_KEY"}]'::jsonb, true),
  -- Codex authenticates via a login step, not pure env: the setup command pipes
  -- the injected key into `codex login`, which writes ~/.codex/auth.json. It is
  -- idempotent (re-login overwrites), so it can run on every push.
  ('openai', 'OpenAI Codex', 'codex', 'printenv OPENAI_API_KEY | codex login --with-api-key',
   '[{"name":"api_key","label":"OpenAI API key","env":"OPENAI_API_KEY"}]'::jsonb, true),
  -- pi.dev has no key of its own; it borrows a model-provider key. We store it
  -- under pi's own path (never read from claude's) to keep per-provider isolation
  -- (Phase 6 decision #2), so the field name is provider-local.
  ('pi', 'Pi', 'pi', NULL,
   '[{"name":"anthropic_api_key","label":"Anthropic API key (used by Pi)","env":"ANTHROPIC_API_KEY"}]'::jsonb, true);
