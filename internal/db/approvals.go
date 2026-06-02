package db

import (
	"database/sql"
	"errors"
	"fmt"
)

// PendingApproval is a held message from an unknown sender awaiting a decision
// (brief §3.4). The stored fields are enough to replay the original message
// through routing once approved.
type PendingApproval struct {
	ID           int64
	Channel      string
	ChatID       string
	SenderID     string
	SenderName   string
	Text         string
	AgentGroupID int64
}

// UpsertPendingApproval records (or refreshes the text of) an outstanding
// approval request for (channel, sender, group), returning its id. A repeat
// message from the same unknown sender updates the stored text rather than
// creating a duplicate.
func (d *DB) UpsertPendingApproval(p PendingApproval) (int64, error) {
	if _, err := d.Exec(`
		INSERT INTO pending_approvals (channel, chat_id, sender_id, sender_name, text, agent_group_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (channel, sender_id, agent_group_id)
		DO UPDATE SET text = excluded.text, chat_id = excluded.chat_id, sender_name = excluded.sender_name`,
		p.Channel, p.ChatID, p.SenderID, p.SenderName, p.Text, p.AgentGroupID,
	); err != nil {
		return 0, fmt.Errorf("upsert pending approval: %w", err)
	}
	var id int64
	if err := d.QueryRow(
		`SELECT id FROM pending_approvals WHERE channel = ? AND sender_id = ? AND agent_group_id = ?`,
		p.Channel, p.SenderID, p.AgentGroupID,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("read pending approval id: %w", err)
	}
	return id, nil
}

// PendingApprovalByID returns a held request, or (nil, nil) if not found.
func (d *DB) PendingApprovalByID(id int64) (*PendingApproval, error) {
	var p PendingApproval
	var name sql.NullString
	err := d.QueryRow(`
		SELECT id, channel, chat_id, sender_id, sender_name, text, agent_group_id
		FROM pending_approvals WHERE id = ?`, id,
	).Scan(&p.ID, &p.Channel, &p.ChatID, &p.SenderID, &name, &p.Text, &p.AgentGroupID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pending approval: %w", err)
	}
	p.SenderName = name.String
	return &p, nil
}

// DeletePendingApproval removes a held request (used on deny, and after a
// successful approve once the message has been replayed).
func (d *DB) DeletePendingApproval(id int64) error {
	if _, err := d.Exec(`DELETE FROM pending_approvals WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete pending approval: %w", err)
	}
	return nil
}

// ApprovePendingApproval makes the held sender a known member (registering a
// user + identity) and returns the original message so the caller can replay it.
// The pending row is deleted. Idempotent on the identity via UpsertUserWithIdentity.
func (d *DB) ApprovePendingApproval(id int64) (*PendingApproval, error) {
	p, err := d.PendingApprovalByID(id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, nil // already handled / unknown id
	}
	name := p.SenderName
	if name == "" {
		name = p.SenderID
	}
	if _, err := d.UpsertUserWithIdentity(name, string(roleMember), p.Channel, p.SenderID); err != nil {
		return nil, fmt.Errorf("approve: register member: %w", err)
	}
	if err := d.DeletePendingApproval(id); err != nil {
		return nil, err
	}
	return p, nil
}

// roleMember mirrors permissions.RoleMember without importing that package
// (db is a lower layer). Kept in sync with internal/permissions.
const roleMember = "member"
