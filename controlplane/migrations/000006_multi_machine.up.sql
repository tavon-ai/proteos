-- Phase: multiple machines per user. Phase 2 made machines 1:1 with users via a
-- UNIQUE constraint on machines.user_id; lift that so a user can run several
-- machines at once. user_id stays (ownership) but is now non-unique, so add an
-- explicit index for the per-user lookups (ListMachinesByUserID / cap count).
-- Machines also gain a display name (auto-named machine-N on create, renameable).

ALTER TABLE machines DROP CONSTRAINT machines_user_id_key;

ALTER TABLE machines ADD COLUMN name text NOT NULL DEFAULT '';

CREATE INDEX idx_machines_user ON machines(user_id);
