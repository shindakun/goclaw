package db

import "fmt"

// Bootstrap holds the optional startup seeding derived from config. It exists so
// the first real user can message the host without hand-editing the DB
// (brief §3.4). All steps are idempotent.
type Bootstrap struct {
	// OwnerTelegramID, if non-empty, seeds an "owner" user with this Telegram
	// identity.
	OwnerTelegramID string
	// DefaultAgentGroup, if non-empty, ensures an agent group with this name and
	// folder exists, to wire conversations to.
	DefaultAgentGroupName   string
	DefaultAgentGroupFolder string
}

// Apply runs the bootstrap. Returns the owner user id (0 if not seeded) and the
// default agent group id (0 if not seeded).
func (d *DB) Apply(b Bootstrap) (ownerID, agentGroupID int64, err error) {
	if b.OwnerTelegramID != "" {
		ownerID, err = d.UpsertUserWithIdentity("owner", "owner", "telegram", b.OwnerTelegramID)
		if err != nil {
			return 0, 0, fmt.Errorf("bootstrap owner: %w", err)
		}
	}
	if b.DefaultAgentGroupName != "" {
		agentGroupID, err = d.upsertAgentGroup(b.DefaultAgentGroupName, b.DefaultAgentGroupFolder)
		if err != nil {
			return 0, 0, fmt.Errorf("bootstrap agent group: %w", err)
		}
	}
	return ownerID, agentGroupID, nil
}

// upsertAgentGroup ensures an agent group with the given name exists, returning
// its id.
func (d *DB) upsertAgentGroup(name, folder string) (int64, error) {
	if _, err := d.Exec(`
		INSERT INTO agent_groups (name, folder) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET folder = excluded.folder`,
		name, folder,
	); err != nil {
		return 0, fmt.Errorf("upsert agent group: %w", err)
	}
	var id int64
	if err := d.QueryRow(`SELECT id FROM agent_groups WHERE name = ?`, name).Scan(&id); err != nil {
		return 0, fmt.Errorf("read agent group id: %w", err)
	}
	return id, nil
}

// EnsureWiring ensures a wiring exists for (messagingGroupID → agentGroupID)
// with the given scope/policy, returning its id. Idempotent. Used both by the
// optional owner auto-wire path and by explicit wiring setup.
func (d *DB) EnsureWiring(messagingGroupID, agentGroupID int64, senderScope, policy string) (int64, error) {
	if _, err := d.Exec(`
		INSERT INTO wirings (messaging_group_id, agent_group_id, sender_scope, unknown_sender_policy)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (messaging_group_id, agent_group_id)
		DO UPDATE SET sender_scope = excluded.sender_scope,
		             unknown_sender_policy = excluded.unknown_sender_policy`,
		messagingGroupID, agentGroupID, senderScope, policy,
	); err != nil {
		return 0, fmt.Errorf("ensure wiring: %w", err)
	}
	var id int64
	if err := d.QueryRow(
		`SELECT id FROM wirings WHERE messaging_group_id = ? AND agent_group_id = ?`,
		messagingGroupID, agentGroupID,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("read wiring id: %w", err)
	}
	return id, nil
}
