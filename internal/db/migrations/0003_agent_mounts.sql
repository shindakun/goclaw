-- 0003_agent_mounts — extra host directories an agent group's runner container
-- mounts, beyond its own sessions dir (brief §6.3). Every entry is validated
-- against the external mount allowlist at launch; an entry that isn't permitted
-- is skipped (fail closed), never silently widened.

CREATE TABLE IF NOT EXISTS agent_mounts (
    id              INTEGER PRIMARY KEY,
    agent_group_id  INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
    host_path       TEXT NOT NULL,   -- host source (validated vs the allowlist)
    container_path  TEXT NOT NULL,   -- absolute path inside the container
    read_write      INTEGER NOT NULL DEFAULT 0, -- 0 = ro, 1 = rw
    UNIQUE (agent_group_id, container_path)
);

CREATE INDEX IF NOT EXISTS idx_agent_mounts_group ON agent_mounts(agent_group_id);
