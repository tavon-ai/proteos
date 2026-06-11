-- Phase 4 schema: per-machine persistent disks and the current hibernation
-- snapshot. The disks table is a seam (Phase 12 backups, future network storage)
-- rather than a column on machines. The snapshots table holds at most ONE row
-- per machine — the current snapshot — deleted when consumed by a resume or
-- invalidated by a cold stop; full history lives in machine_events.

CREATE TABLE disks (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    machine_id  uuid NOT NULL UNIQUE REFERENCES machines(id) ON DELETE CASCADE,
    size_mib    integer NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- The machine's attached disk (set at create) and how its current/last run
-- started ('cold' | 'resumed'). boot is durable so the API summary can report
-- it without scanning the event log; the same value is also recorded in the
-- machine_events payload for audit.
ALTER TABLE machines ADD COLUMN disk_id uuid REFERENCES disks(id);
ALTER TABLE machines ADD COLUMN boot text;

CREATE TABLE snapshots (
    machine_id  uuid PRIMARY KEY REFERENCES machines(id) ON DELETE CASCADE,
    fc_version  text NOT NULL,
    mem_bytes   bigint NOT NULL,
    kernel_ref  text NOT NULL,
    rootfs_ref  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);
