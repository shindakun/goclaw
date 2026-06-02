-- 0001_init — initial central schema for the goclaw host.
-- Mirrors the brief's v2 schema so an existing install's data migrates 1:1 (brief §5).
--
-- Migrations are pure DDL: connection pragmas (WAL, foreign_keys) are applied
-- per-connection in db.go, and each migration file runs inside a transaction,
-- so do NOT put PRAGMA journal_mode here.

-- Users known to the host, resolved from channel-native sender ids.
CREATE TABLE IF NOT EXISTS users (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,
    role        TEXT NOT NULL DEFAULT 'member', -- owner | admin | member
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- A channel-native identity (e.g. a Telegram numeric id) mapped to a user.
CREATE TABLE IF NOT EXISTS user_identities (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel     TEXT NOT NULL,                  -- "telegram", "slack", ...
    sender_id   TEXT NOT NULL,                  -- channel-native, stable id
    UNIQUE (channel, sender_id)
);

-- A conversation on a channel (a Telegram chat, a Slack channel, ...).
CREATE TABLE IF NOT EXISTS messaging_groups (
    id          INTEGER PRIMARY KEY,
    channel     TEXT NOT NULL,
    chat_id     TEXT NOT NULL,                  -- channel-native conversation id
    title       TEXT,
    UNIQUE (channel, chat_id)
);

-- An agent group: a per-container unit with its own filesystem under groups/.
CREATE TABLE IF NOT EXISTS agent_groups (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    folder      TEXT NOT NULL,                  -- groups/<folder>/
    provider    TEXT NOT NULL DEFAULT 'claude',
    runtime     TEXT NOT NULL DEFAULT 'crun',   -- crun | gvisor | kata
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Wiring: which messaging group routes into which agent group, and the scope.
CREATE TABLE IF NOT EXISTS wirings (
    id                    INTEGER PRIMARY KEY,
    messaging_group_id    INTEGER NOT NULL REFERENCES messaging_groups(id) ON DELETE CASCADE,
    agent_group_id        INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
    sender_scope          TEXT NOT NULL DEFAULT 'all',     -- all | known | owner
    unknown_sender_policy TEXT NOT NULL DEFAULT 'strict',  -- public | strict | request_approval
    UNIQUE (messaging_group_id, agent_group_id)
);

-- Sessions: each gets isolated inbound/outbound DBs under data/v2-sessions/.
CREATE TABLE IF NOT EXISTS sessions (
    id              INTEGER PRIMARY KEY,
    agent_group_id  INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
    session_key     TEXT NOT NULL,              -- {session_id} on disk
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    last_active_at  TEXT,
    UNIQUE (agent_group_id, session_key)
);

-- Channels an agent group may deliver to beyond its origin chat (delivery auth, §9).
CREATE TABLE IF NOT EXISTS agent_destinations (
    id              INTEGER PRIMARY KEY,
    agent_group_id  INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
    channel         TEXT NOT NULL,
    chat_id         TEXT NOT NULL,
    UNIQUE (agent_group_id, channel, chat_id)
);
