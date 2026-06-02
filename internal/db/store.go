package db

import (
	"database/sql"
	"errors"
	"fmt"
)

// This file holds the central-DB query helpers the router uses to resolve an
// inbound message's routing chain: user → messaging group → agent group →
// session (brief §3.2). Resolution failures return (nil/zero, nil) so the
// caller can distinguish "not found" from a real DB error.

// User is a resolved host user.
type User struct {
	ID   int64
	Name string
	Role string // owner | admin | member
}

// MessagingGroup is a resolved conversation.
type MessagingGroup struct {
	ID      int64
	Channel string
	ChatID  string
	Title   string
}

// Wiring is a resolved messaging-group → agent-group route with its access policy.
type Wiring struct {
	ID                  int64
	MessagingGroupID    int64
	AgentGroupID        int64
	SenderScope         string // all | known | owner
	UnknownSenderPolicy string // public | strict | request_approval
}

// UserByIdentity resolves the user owning a (channel, senderID) identity.
// Returns (nil, nil) when no such identity is registered.
func (d *DB) UserByIdentity(channel, senderID string) (*User, error) {
	var u User
	err := d.QueryRow(`
		SELECT u.id, u.name, u.role
		FROM user_identities ui
		JOIN users u ON u.id = ui.user_id
		WHERE ui.channel = ? AND ui.sender_id = ?`,
		channel, senderID,
	).Scan(&u.ID, &u.Name, &u.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve user by identity: %w", err)
	}
	return &u, nil
}

// MessagingGroupByChat resolves a conversation by (channel, chatID).
// Returns (nil, nil) when the conversation isn't registered.
func (d *DB) MessagingGroupByChat(channel, chatID string) (*MessagingGroup, error) {
	var mg MessagingGroup
	var title sql.NullString
	err := d.QueryRow(`
		SELECT id, channel, chat_id, title
		FROM messaging_groups
		WHERE channel = ? AND chat_id = ?`,
		channel, chatID,
	).Scan(&mg.ID, &mg.Channel, &mg.ChatID, &title)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve messaging group: %w", err)
	}
	mg.Title = title.String
	return &mg, nil
}

// HasAgentDestination reports whether an agent group has an explicit
// agent_destinations row for (channel, chatID). Used by delivery authorization
// for non-origin targets (brief §9).
func (d *DB) HasAgentDestination(agentGroupID int64, channel, chatID string) (bool, error) {
	var n int
	err := d.QueryRow(`
		SELECT count(*) FROM agent_destinations
		WHERE agent_group_id = ? AND channel = ? AND chat_id = ?`,
		agentGroupID, channel, chatID,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check agent destination: %w", err)
	}
	return n > 0, nil
}

// Session is a resolved session row (the central-DB record; the on-disk DB pair
// is opened separately via OpenSession).
type Session struct {
	ID           int64
	AgentGroupID int64
	SessionKey   string
}

// ActiveSessions returns every session row. v0 treats all sessions as drainable;
// a later refinement filters by recent activity (brief §3.3 sweep). The delivery
// loop uses this to know which outbound.db files to poll.
func (d *DB) ActiveSessions() ([]Session, error) {
	rows, err := d.Query(`SELECT id, agent_group_id, session_key FROM sessions`)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.AgentGroupID, &s.SessionKey); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ResolveOrCreateSession returns the session for (agentGroupID, sessionKey),
// creating its central-DB row on first use and bumping last_active_at. The
// on-disk inbound/outbound DBs are opened by the caller via OpenSession.
// v0 uses one session per conversation, so sessionKey is the origin chat id.
func (d *DB) ResolveOrCreateSession(agentGroupID int64, sessionKey string) (*Session, error) {
	if _, err := d.Exec(`
		INSERT INTO sessions (agent_group_id, session_key, last_active_at)
		VALUES (?, ?, datetime('now'))
		ON CONFLICT (agent_group_id, session_key)
		DO UPDATE SET last_active_at = datetime('now')`,
		agentGroupID, sessionKey,
	); err != nil {
		return nil, fmt.Errorf("resolve-or-create session: %w", err)
	}
	var s Session
	if err := d.QueryRow(
		`SELECT id, agent_group_id, session_key FROM sessions WHERE agent_group_id = ? AND session_key = ?`,
		agentGroupID, sessionKey,
	).Scan(&s.ID, &s.AgentGroupID, &s.SessionKey); err != nil {
		return nil, fmt.Errorf("read session: %w", err)
	}
	return &s, nil
}

// WiringForMessagingGroup resolves the wiring for a messaging group. v0 assumes
// at most one wiring per messaging group; returns (nil, nil) when unwired.
func (d *DB) WiringForMessagingGroup(messagingGroupID int64) (*Wiring, error) {
	var w Wiring
	err := d.QueryRow(`
		SELECT id, messaging_group_id, agent_group_id, sender_scope, unknown_sender_policy
		FROM wirings
		WHERE messaging_group_id = ?
		LIMIT 1`,
		messagingGroupID,
	).Scan(&w.ID, &w.MessagingGroupID, &w.AgentGroupID, &w.SenderScope, &w.UnknownSenderPolicy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve wiring: %w", err)
	}
	return &w, nil
}
