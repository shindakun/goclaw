-- Outbound message queue for one session. The CONTAINER is the sole writer; the
-- host's delivery loop polls this table, authorizes, dispatches, and marks rows
-- delivered (brief §3.1, §9). The host opens this read-mostly (it only updates
-- the delivery status), so it never contends with the container's writes.
--
-- Pure DDL: connection pragmas are applied per-connection in db.go.

CREATE TABLE IF NOT EXISTS messages (
    id            INTEGER PRIMARY KEY,
    -- where the reply should go. The container sets these; delivery authorizes
    -- them against origin-chat + agent_destinations before dispatch (brief §9).
    channel       TEXT NOT NULL,
    chat_id       TEXT NOT NULL,
    text          TEXT NOT NULL,
    -- the nature of this reply, so the host can log it distinctly: 'reply' (a
    -- normal agent answer) or 'turn_failed' (the runner gave up after repeated
    -- transient failures; the text is an apology and the host emits a
    -- runner.turn_failed event on delivery). Default keeps existing rows 'reply'.
    kind          TEXT NOT NULL DEFAULT 'reply',
    -- for a 'turn_failed' row, the origin of the message that failed, echoed from the
    -- inbound row so the host can re-arm a failed scheduled job: 'user', 'task:<id>',
    -- or 'maint:<name>'. 'user' for a normal reply. See inbound.sql's source column.
    source        TEXT NOT NULL DEFAULT 'user',
    -- lifecycle: pending → delivered (or failed). The container writes
    -- 'pending'; the host delivery loop advances it.
    status        TEXT NOT NULL DEFAULT 'pending',
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    delivered_at  TEXT,
    error         TEXT
);

CREATE INDEX IF NOT EXISTS idx_messages_status ON messages(status);

-- Per-session key/value scratch owned by the runner. Used to persist the Claude
-- conversation session id so multi-turn context survives across the runner's
-- open-per-poll cycles (e.g. claude_session_id). The runner is the sole writer.
CREATE TABLE IF NOT EXISTS meta (
    key    TEXT PRIMARY KEY,
    value  TEXT NOT NULL
);
