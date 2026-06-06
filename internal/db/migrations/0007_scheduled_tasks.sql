-- 0007_scheduled_tasks - user-definable recurring agent work (docs/scheduled-tasks.md).
-- Generalizes the hardcoded vault-maintenance jobs into DB rows: each task carries its
-- own schedule (period_days + at_hour, or every_seconds for pure-interval), its own
-- target (where it runs and where the reply is delivered), an owner (authorization +
-- listing), and a prompt the scheduler enqueues for the agent.
--
-- Last-run tracking stays in the kv table keyed by task id (reusing the maintenance
-- scheduler's due() logic), so this table holds only the definition.
--
-- Pure DDL: connection pragmas are applied per-connection in db.go.

CREATE TABLE IF NOT EXISTS scheduled_tasks (
    id             TEXT PRIMARY KEY,            -- uuid
    name           TEXT NOT NULL,               -- human label, unique per owner
    owner_user_id  INTEGER NOT NULL,            -- who created it (authorization + listing)
    agent_group_id INTEGER NOT NULL,            -- where it runs
    session_key    TEXT NOT NULL,               -- which session (its own multi-turn context)
    channel        TEXT NOT NULL,               -- delivery channel for the reply
    chat_id        TEXT NOT NULL,               -- delivery chat
    period_days    INTEGER NOT NULL DEFAULT 1,  -- 1 = daily, 7 = weekly
    at_hour        INTEGER NOT NULL DEFAULT -1, -- local hour 0-23; <0 = use every_seconds
    every_seconds  INTEGER NOT NULL DEFAULT 0,  -- pure-interval fallback when at_hour < 0
    prompt         TEXT NOT NULL,               -- the instruction enqueued for the agent
    enabled        INTEGER NOT NULL DEFAULT 1,  -- 0 = paused (kept, never fires)
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),

    FOREIGN KEY (owner_user_id)  REFERENCES users(id),
    FOREIGN KEY (agent_group_id) REFERENCES agent_groups(id)
);

-- A task name is unique per owner (so "/schedule remove inbox-summary" is unambiguous).
CREATE UNIQUE INDEX IF NOT EXISTS idx_scheduled_tasks_owner_name
    ON scheduled_tasks(owner_user_id, name);
