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

// AgentMount is an extra host directory an agent group wants mounted into its
// runner, before allowlist validation (brief §6.3).
type AgentMount struct {
	HostPath      string
	ContainerPath string
	ReadWrite     bool
}

// AgentMounts returns the extra mounts requested by an agent group. These are
// validated against the external allowlist at launch; the DB only records the
// request.
func (d *DB) AgentMounts(agentGroupID int64) ([]AgentMount, error) {
	rows, err := d.Query(
		`SELECT host_path, container_path, read_write FROM agent_mounts WHERE agent_group_id = ?`,
		agentGroupID)
	if err != nil {
		return nil, fmt.Errorf("list agent mounts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AgentMount
	for rows.Next() {
		var m AgentMount
		var rw int
		if err := rows.Scan(&m.HostPath, &m.ContainerPath, &rw); err != nil {
			return nil, err
		}
		m.ReadWrite = rw != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// Identity is a channel-native identity for a user (used to message them).
type Identity struct {
	Channel  string
	SenderID string
}

// OwnerUserID returns the id of an owner user (v0: any owner; agent-group-scoped owners
// later). found=false if there is no owner. Used to attribute an agent-emitted action
// (e.g. a /schedule directive in the agent's reply) to the conversation's owner.
func (d *DB) OwnerUserID() (id int64, found bool, err error) {
	err = d.QueryRow(`SELECT id FROM users WHERE role = 'owner' ORDER BY id LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("owner user id: %w", err)
	}
	return id, true, nil
}

// OwnerIdentity returns a channel identity for an owner user, so the host can
// send them the approval card (brief §3.4). v0 picks any owner's first identity;
// agent-group-scoped owners can be added later. Returns (nil, nil) if no owner
// has a known identity.
func (d *DB) OwnerIdentity(agentGroupID int64) (*Identity, error) {
	var idn Identity
	err := d.QueryRow(`
		SELECT ui.channel, ui.sender_id
		FROM users u
		JOIN user_identities ui ON ui.user_id = u.id
		WHERE u.role = 'owner'
		ORDER BY u.id
		LIMIT 1`,
	).Scan(&idn.Channel, &idn.SenderID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("owner identity: %w", err)
	}
	return &idn, nil
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

// GetKV returns a host-state value, or ("", false) if unset. Backed by the
// central DB's kv table (e.g. maintenance-job last-run timestamps).
func (d *DB) GetKV(key string) (string, bool, error) {
	var v string
	err := d.QueryRow(`SELECT value FROM kv WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get kv %q: %w", key, err)
	}
	return v, true, nil
}

// SetKV upserts a host-state value.
func (d *DB) SetKV(key, value string) error {
	_, err := d.Exec(`
		INSERT INTO kv (key, value, updated_at) VALUES (?, ?, datetime('now'))
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = datetime('now')`,
		key, value)
	if err != nil {
		return fmt.Errorf("set kv %q: %w", key, err)
	}
	return nil
}

// Session is a resolved session row (the central-DB record; the on-disk DB pair
// is opened separately via OpenSession).
type Session struct {
	ID           int64
	AgentGroupID int64
	SessionKey   string
}

// CountAgentGroups returns how many agent groups exist. The host uses it to gate
// the shared operational event-log mount fail-closed: the single event log can
// contain events about every group, so it is safe to mount read-only into a
// group's container ONLY when there is exactly one group (nothing another group
// owns can be in it). With more than one group the host refuses the mount until a
// per-group event log is built (RFC event-log-and-introspection §7 open question 1).
func (d *DB) CountAgentGroups() (int, error) {
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM agent_groups`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count agent groups: %w", err)
	}
	return n, nil
}

// ActiveSessions returns every session row. v0 treats all sessions as drainable;
// a later refinement filters by recent activity (brief §3.3 sweep). The delivery
// loop uses this to know which outbound.db files to poll.
func (d *DB) ActiveSessions() ([]Session, error) {
	rows, err := d.Query(`SELECT id, agent_group_id, session_key FROM sessions`)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

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

// AgentGroupIDsActiveSince returns the set of agent group ids that have at least
// one session active at or after the given cutoff (RFC-ish "YYYY-MM-DD HH:MM:SS"
// in UTC, matching datetime('now')). Used by the sweep to decide which group
// runners are still wanted (brief §3.3).
func (d *DB) AgentGroupIDsActiveSince(cutoff string) (map[int64]bool, error) {
	rows, err := d.Query(
		`SELECT DISTINCT agent_group_id FROM sessions WHERE last_active_at >= ?`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("active groups since: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
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
