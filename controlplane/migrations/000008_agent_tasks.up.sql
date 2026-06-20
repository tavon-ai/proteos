-- AT1: headless agent tasks. A task is orchestration state the guest filesystem
-- cannot hold (status lifecycle, the agent's own session id for resume, usage),
-- so the control plane owns it. The agent run only ever produces a dirty working
-- tree; commit/push/PR remain separate, explicit (GR) actions — there is no
-- status here for those.
CREATE TABLE agent_tasks (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    machine_id       uuid NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
    user_id          uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider         text NOT NULL,
    project          text NOT NULL,
    prompt           text NOT NULL,
    status           text NOT NULL DEFAULT 'queued'
                     CHECK (status IN ('queued','running','done','failed','canceled')),
    agent_session_id text NOT NULL DEFAULT '',     -- the coding agent's own session id (resume)
    usage            jsonb NOT NULL DEFAULT '{}'::jsonb, -- {cost_usd, num_turns, duration_ms}
    result_summary   text NOT NULL DEFAULT '',      -- the agent's final result text (capped)
    error            text NOT NULL DEFAULT '',      -- sanitized failure detail when failed
    created_at       timestamptz NOT NULL DEFAULT now(),
    started_at       timestamptz,                   -- when the run was dispatched (running)
    ended_at         timestamptz                    -- when it reached a terminal state
);

CREATE INDEX idx_agent_tasks_machine ON agent_tasks(machine_id, created_at DESC);
