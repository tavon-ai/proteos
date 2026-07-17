-- TAV-116: per-machine network policy. One row per machine (1:1, like disks),
-- created lazily on first PUT; a machine with no row behaves as "allow_all"
-- (the API/service layer default — see machine.DefaultNetworkPolicyMode).
-- domains is only meaningful for allow_domains/deny_domains; it is an empty
-- array otherwise.
CREATE TABLE network_policies (
    machine_id uuid        PRIMARY KEY REFERENCES machines(id) ON DELETE CASCADE,
    mode       text        NOT NULL DEFAULT 'allow_all'
               CHECK (mode IN ('allow_all', 'deny_all', 'allow_domains', 'deny_domains')),
    domains    jsonb       NOT NULL DEFAULT '[]'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
