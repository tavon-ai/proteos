-- Reverse Phase 6's registry generalization, restoring the Phase 5 shape.
DELETE FROM providers WHERE key IN ('gemini', 'openai', 'pi');

ALTER TABLE providers ADD COLUMN secret_env jsonb NOT NULL DEFAULT '{}'::jsonb;
UPDATE providers
SET secret_env = '{"ANTHROPIC_API_KEY":"api_key"}'::jsonb
WHERE key = 'claude';

ALTER TABLE providers DROP COLUMN secret_fields;
ALTER TABLE providers DROP COLUMN setup_command;
