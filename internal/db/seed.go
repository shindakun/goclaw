package db

import "fmt"

// This file holds write helpers for bootstrapping and first-contact records:
// registering a user identity, and recording a conversation the first time it
// is seen.

// UpsertUserWithIdentity ensures a user named name with the given role exists
// and is linked to (channel, senderID). It is idempotent: re-running with the
// same identity updates the existing user's name/role rather than duplicating.
// Returns the user id. Used by the owner bootstrap (brief §3.4 permissions).
func (d *DB) UpsertUserWithIdentity(name, role, channel, senderID string) (int64, error) {
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// Does this identity already map to a user?
	var userID int64
	err = tx.QueryRow(
		`SELECT user_id FROM user_identities WHERE channel = ? AND sender_id = ?`,
		channel, senderID,
	).Scan(&userID)

	switch {
	case err == nil:
		// Identity exists - update the user's name/role to match.
		if _, err := tx.Exec(`UPDATE users SET name = ?, role = ? WHERE id = ?`, name, role, userID); err != nil {
			return 0, fmt.Errorf("update user: %w", err)
		}
	default:
		// New identity - create the user and the identity row.
		res, err := tx.Exec(`INSERT INTO users (name, role) VALUES (?, ?)`, name, role)
		if err != nil {
			return 0, fmt.Errorf("insert user: %w", err)
		}
		userID, err = res.LastInsertId()
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(
			`INSERT INTO user_identities (user_id, channel, sender_id) VALUES (?, ?, ?)`,
			userID, channel, senderID,
		); err != nil {
			return 0, fmt.Errorf("insert identity: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return userID, nil
}

// AddIdentity links an existing user to (channel, senderID), so one user can be
// reachable on multiple channels (e.g. the owner on Telegram and Discord).
// Idempotent: if the identity already exists it is re-pointed at userID. Returns
// nil on success.
func (d *DB) AddIdentity(userID int64, channel, senderID string) error {
	if _, err := d.Exec(`
		INSERT INTO user_identities (user_id, channel, sender_id) VALUES (?, ?, ?)
		ON CONFLICT (channel, sender_id) DO UPDATE SET user_id = excluded.user_id`,
		userID, channel, senderID,
	); err != nil {
		return fmt.Errorf("add identity: %w", err)
	}
	return nil
}

// UpsertMessagingGroup records (or refreshes the title of) a conversation,
// returning its id. Called on first contact so unknown chats become routable.
func (d *DB) UpsertMessagingGroup(channel, chatID, title string) (int64, error) {
	if _, err := d.Exec(`
		INSERT INTO messaging_groups (channel, chat_id, title)
		VALUES (?, ?, ?)
		ON CONFLICT (channel, chat_id) DO UPDATE SET title = excluded.title`,
		channel, chatID, title,
	); err != nil {
		return 0, fmt.Errorf("upsert messaging group: %w", err)
	}
	var id int64
	if err := d.QueryRow(
		`SELECT id FROM messaging_groups WHERE channel = ? AND chat_id = ?`,
		channel, chatID,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("read messaging group id: %w", err)
	}
	return id, nil
}
