-- Inbound message queue for one session. The HOST is the sole writer; the
-- container polls this table and marks rows consumed (brief §3.1, §5.1).
--
-- Pure DDL: connection pragmas (WAL, foreign_keys, busy_timeout) are applied
-- per-connection in db.go. Do not put PRAGMA journal_mode here.

CREATE TABLE IF NOT EXISTS messages (
    id            INTEGER PRIMARY KEY,
    -- routing/context the container needs to produce + address a reply.
    channel       TEXT NOT NULL,   -- origin channel ("telegram", ...)
    chat_id       TEXT NOT NULL,   -- origin conversation id
    sender_id     TEXT NOT NULL,   -- channel-native sender id
    sender_name   TEXT,            -- best-effort display name
    text          TEXT NOT NULL,   -- message body
    -- lifecycle: pending → consumed. The host writes 'pending'; the container
    -- flips it to 'consumed' once it has picked the message up.
    status        TEXT NOT NULL DEFAULT 'pending',
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    consumed_at   TEXT
);

CREATE INDEX IF NOT EXISTS idx_messages_status ON messages(status);
