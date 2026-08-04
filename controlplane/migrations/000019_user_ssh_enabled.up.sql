-- Account-wide master switch for inbound SSH (migration 000018 added the keys
-- themselves). false (the default) ⇒ the user's registered login keys are NOT
-- rendered into ~/.ssh/authorized_keys on any of their machines, and /gw/ssh
-- refuses the tunnel. true ⇒ both are allowed.
--
-- Default false is deliberate: inbound access is opt-in, matching the
-- fail-closed posture the rest of the guest network path takes (the node-agent's
-- default-deny nft policy). Registering a key is not on its own consent to be
-- reachable — the user turns the switch on.
ALTER TABLE users ADD COLUMN ssh_enabled boolean NOT NULL DEFAULT false;
