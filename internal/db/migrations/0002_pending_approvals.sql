-- 0002_pending_approvals - holds messages from unknown senders awaiting an
-- owner/admin decision under the request_approval policy (brief §3.4).
--
-- On request: a row is created and an approval card is sent to the owner.
-- On approve: the sender becomes a group member and the stored message is
--   replayed through routing.
-- On deny: the row is deleted; a future message re-triggers the flow.

CREATE TABLE IF NOT EXISTS pending_approvals (
    id                 INTEGER PRIMARY KEY,
    -- where the request originated, so the message can be replayed verbatim.
    channel            TEXT NOT NULL,
    chat_id            TEXT NOT NULL,
    sender_id          TEXT NOT NULL,
    sender_name        TEXT,
    text               TEXT NOT NULL,
    -- the agent group the sender is requesting access to (resolved at request).
    agent_group_id     INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
    created_at         TEXT NOT NULL DEFAULT (datetime('now')),
    -- one outstanding request per (channel, sender, group); a repeat message
    -- updates the stored text rather than piling up duplicates.
    UNIQUE (channel, sender_id, agent_group_id)
);

CREATE INDEX IF NOT EXISTS idx_pending_approvals_group ON pending_approvals(agent_group_id);
