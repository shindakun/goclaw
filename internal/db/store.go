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
