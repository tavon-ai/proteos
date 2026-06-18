-- Reverse the multi-machine migration. Restoring the UNIQUE constraint requires
-- the data to once again be 1:1 per user (any user with >1 machine must be
-- reduced first, or this will fail) — acceptable for a dev rollback.

DROP INDEX IF EXISTS idx_machines_user;

ALTER TABLE machines DROP COLUMN name;

ALTER TABLE machines ADD CONSTRAINT machines_user_id_key UNIQUE (user_id);
