-- Phase 2 schema: hosts, machines, and the machine_events audit log. Machines
-- are 1:1 with users this phase. Every machine state change is recorded as a
-- machine_events row in the same transaction as the state update (see
-- internal/machine), so the audit log can never drift from the machine row.

CREATE TABLE hosts (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE,
    agent_url   text NOT NULL,
    status      text NOT NULL DEFAULT 'active',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE machines (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE, -- 1:1
    state          text NOT NULL DEFAULT 'requested'
                   CHECK (state IN ('requested','provisioning','running','starting',
                                    'stopping','hibernating','stopped','error')),
    host_id        uuid REFERENCES hosts(id),
    vm_handle      text,
    guest_ip       inet,
    kernel_ref     text NOT NULL,
    rootfs_ref     text NOT NULL,
    resource_spec  jsonb NOT NULL,          -- {"vcpus":2,"mem_mib":2048}
    last_error     text,
    last_active_at timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE machine_events (
    id          bigserial PRIMARY KEY,      -- doubles as SSE Last-Event-ID
    machine_id  uuid NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
    type        text NOT NULL,              -- 'transition' | 'error' | 'info'
    from_state  text,
    to_state    text,
    actor       text NOT NULL,              -- 'user:<uuid>' | 'system:poller' | 'system:api'
    payload     jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_machine_events_machine ON machine_events(machine_id, id);
