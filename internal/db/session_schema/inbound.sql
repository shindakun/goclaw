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

-- Delivery ledger. The host records which outbound rows it has already
-- dispatched, so a reply is delivered exactly once even if a status write to
-- the runner-owned outbound.db is lost across the bind mount.
--
-- This lives in inbound.db ON PURPOSE: inbound.db is the host-owned file, so the
-- host is the sole writer of delivery state and the runner never touches it
-- (brief §5.1). Tracking delivery in outbound.db instead would make the host a
-- second writer of the runner's file - the page caches aren't coherent across
-- the podman VM mount, so the runner's next write resurrects the host's
-- 'delivered' row back to 'pending' and the reply is sent twice. Keyed by the
-- outbound message id; INSERT OR IGNORE makes re-delivery a no-op.
CREATE TABLE IF NOT EXISTS delivered (
    outbound_id   INTEGER PRIMARY KEY,  -- messages.id from outbound.db
    status        TEXT NOT NULL DEFAULT 'delivered',  -- 'delivered' | 'failed'
    error         TEXT,                 -- failure reason when status='failed'
    delivered_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
