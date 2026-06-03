-- 0002_kv - a small key/value store on the central DB for host-side scheduler
-- state (e.g. when a maintenance job last ran), so schedules survive restarts and
-- don't double-fire.
--
-- Pure DDL: connection pragmas are applied per-connection in db.go.

CREATE TABLE IF NOT EXISTS kv (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
