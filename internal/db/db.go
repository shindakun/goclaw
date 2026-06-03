// Package db owns the central goclaw database and the per-session
// inbound/outbound databases. It uses the pure-Go modernc.org/sqlite driver so
// the host keeps its single static-binary story (brief §5). WAL mode is enabled
// on every handle, and writer handles are capped to a single connection to
// preserve the single-writer-per-DB invariant (brief §5.1).
package db

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

//go:embed session_schema/inbound.sql
var inboundSchema string

//go:embed session_schema/outbound.sql
var outboundSchema string

// DB wraps the central database connection.
type DB struct {
	*sql.DB
	path string
}

// Open opens (creating if needed) the central database at path, enables WAL,
// and applies migrations.
func Open(path string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open central db %q: %w", path, err)
	}
	// The central DB is host-only (one process), so WAL is fine and faster.
	if err := applyPragmas(sqlDB, journalWAL); err != nil {
		sqlDB.Close()
		return nil, err
	}
	d := &DB{DB: sqlDB, path: path}
	if err := d.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate central db: %w", err)
	}
	return d, nil
}

// journalMode selects the SQLite journaling mode for a handle.
type journalMode string

const (
	// journalWAL is fastest but relies on shared memory (-shm) + mmap, which do
	// NOT work across a container bind mount (esp. the macOS podman-machine VM):
	// a writer's changes never become visible to a reader in the other process.
	// Use only for host-only DBs.
	journalWAL journalMode = "WAL"
	// journalDelete is the classic rollback journal. It writes through the main
	// .db file with no -shm dependency, so a write by one process is visible to a
	// reader in another across a bind mount. Use for the session inbound/outbound
	// DBs that the host and the container share (brief §3.1, §5.1).
	journalDelete journalMode = "DELETE"
)

// applyPragmas sets the journal mode, foreign keys, and busy timeout on a fresh
// handle.
func applyPragmas(sqlDB *sql.DB, journal journalMode) error {
	for _, p := range []string{
		"PRAGMA journal_mode = " + string(journal) + ";",
		"PRAGMA foreign_keys = ON;",
		"PRAGMA busy_timeout = 5000;",
	} {
		if _, err := sqlDB.Exec(p); err != nil {
			return fmt.Errorf("pragma %q: %w", p, err)
		}
	}
	return nil
}

// SessionDBs holds the two per-session handles. Each session sees only its own
// inbound/outbound pair (brief §3.4, session isolation).
//
// Ownership convention (brief §5.1):
//   - Inbound is WRITTEN by the host (router) and read by the container.
//   - Outbound is WRITTEN by the container and read by the host (delivery).
//
// We cap the host's writable handle to one connection so the single-writer
// invariant holds even under concurrent goroutines.
type SessionDBs struct {
	Inbound  *sql.DB // host is the writer
	Outbound *sql.DB // host is a reader (the container writes)
	dir      string
}

// AgentGroupDir returns the on-disk directory holding ALL of an agent group's
// session subdirectories: {baseDir}/sessions/{agentGroupID}/. This is what a
// per-agent-group runner container mounts; it then serves every session
// subdirectory beneath it.
func AgentGroupDir(baseDir string, agentGroupID int64) string {
	return filepath.Join(baseDir, "sessions", fmt.Sprintf("%d", agentGroupID))
}

// ClaudeHomeDir returns the host directory that persists a group's Claude CLI
// home (~/.claude) across container restarts: {baseDir}/claude-home/{id}/. The
// CLI stores conversation history there, so persisting it is what makes
// multi-turn --resume survive a container being recreated. Kept OUTSIDE the
// sessions tree so the runner's session scan never visits it.
func ClaudeHomeDir(baseDir string, agentGroupID int64) string {
	return filepath.Join(baseDir, "claude-home", fmt.Sprintf("%d", agentGroupID))
}

// SessionDir returns the on-disk directory for a session's DB pair:
// {baseDir}/sessions/{agentGroupID}/{sessionKey}/. The session
// key derives from external input (a chat id), so it is sanitized to a safe
// single path segment - no separators or traversal can escape the base dir.
func SessionDir(baseDir string, agentGroupID int64, sessionKey string) string {
	return filepath.Join(AgentGroupDir(baseDir, agentGroupID), sanitizeSessionKey(sessionKey))
}

// sanitizeSessionKey reduces a session key to a single safe path segment.
// Anything that isn't an alphanumeric, '-', '_', or '.' becomes '_', and the
// result can never be empty, "." or "..".
func sanitizeSessionKey(key string) string {
	b := make([]rune, 0, len(key))
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b = append(b, r)
		default:
			b = append(b, '_')
		}
	}
	s := string(b)
	if s == "" || s == "." || s == ".." {
		return "_" + s
	}
	return s
}

// OpenSession creates the session directory if needed and opens the
// inbound/outbound pair under the host's data dir.
func OpenSession(baseDir string, agentGroupID int64, sessionKey string) (*SessionDBs, error) {
	return OpenSessionDir(SessionDir(baseDir, agentGroupID, sessionKey))
}

// OpenSessionDir opens (creating + initializing schemas if needed) the
// inbound/outbound pair in an explicit directory. The in-container agent-runner
// uses this against its mounted session dir, since it sees only its own two DBs
// and knows nothing of agent-group ids or the central DB (brief §3.1, §5.1).
// The inbound handle is capped to one connection (single-writer, brief §5.1).
func OpenSessionDir(dir string) (*SessionDBs, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create session dir %q: %w", dir, err)
	}

	inbound, err := sql.Open("sqlite", filepath.Join(dir, "inbound.db"))
	if err != nil {
		return nil, fmt.Errorf("open inbound.db: %w", err)
	}
	// Session DBs cross the container bind mount, so they must NOT use WAL -
	// use the rollback journal so cross-process writes are visible (brief §5.1).
	if err := applyPragmas(inbound, journalDelete); err != nil {
		inbound.Close()
		return nil, err
	}
	inbound.SetMaxOpenConns(1)
	if _, err := inbound.Exec(inboundSchema); err != nil {
		inbound.Close()
		return nil, fmt.Errorf("init inbound schema: %w", err)
	}

	outbound, err := sql.Open("sqlite", filepath.Join(dir, "outbound.db"))
	if err != nil {
		inbound.Close()
		return nil, fmt.Errorf("open outbound.db: %w", err)
	}
	if err := applyPragmas(outbound, journalDelete); err != nil {
		inbound.Close()
		outbound.Close()
		return nil, err
	}
	if _, err := outbound.Exec(outboundSchema); err != nil {
		inbound.Close()
		outbound.Close()
		return nil, fmt.Errorf("init outbound schema: %w", err)
	}

	return &SessionDBs{Inbound: inbound, Outbound: outbound, dir: dir}, nil
}

// EnqueueInbound writes one message into the session's inbound queue as
// 'pending' for the container to pick up. Host is the sole writer (brief §5.1).
func (s *SessionDBs) EnqueueInbound(channel, chatID, senderID, senderName, text string) (int64, error) {
	res, err := s.Inbound.Exec(`
		INSERT INTO messages (channel, chat_id, sender_id, sender_name, text)
		VALUES (?, ?, ?, ?, ?)`,
		channel, chatID, senderID, senderName, text,
	)
	if err != nil {
		return 0, fmt.Errorf("enqueue inbound: %w", err)
	}
	return res.LastInsertId()
}

// OutboundMessage is one row read from a session's outbound queue.
type OutboundMessage struct {
	ID      int64
	Channel string
	ChatID  string
	Text    string
}

// PendingOutbound returns the outbound rows still awaiting delivery, oldest
// first. The host reads these (the container is the writer).
func (s *SessionDBs) PendingOutbound() ([]OutboundMessage, error) {
	rows, err := s.Outbound.Query(`
		SELECT id, channel, chat_id, text
		FROM messages
		WHERE status = 'pending'
		ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("read pending outbound: %w", err)
	}
	defer rows.Close()

	var out []OutboundMessage
	for rows.Next() {
		var m OutboundMessage
		if err := rows.Scan(&m.ID, &m.Channel, &m.ChatID, &m.Text); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MarkOutboundDelivered flips an outbound row to 'delivered'.
func (s *SessionDBs) MarkOutboundDelivered(id int64) error {
	_, err := s.Outbound.Exec(
		`UPDATE messages SET status = 'delivered', delivered_at = datetime('now') WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("mark outbound delivered: %w", err)
	}
	return err
}

// MarkOutboundFailed flips an outbound row to 'failed' with an error message.
func (s *SessionDBs) MarkOutboundFailed(id int64, reason string) error {
	_, err := s.Outbound.Exec(
		`UPDATE messages SET status = 'failed', error = ? WHERE id = ?`, reason, id)
	if err != nil {
		return fmt.Errorf("mark outbound failed: %w", err)
	}
	return err
}

// --- Container/runner side -------------------------------------------------
//
// These are used by the in-container agent-runner, not the host. They live here
// so both sides share one schema definition, but note the ownership inversion:
// the runner is the WRITER of outbound and the consumer of inbound, mirror image
// of the host (brief §5.1).

// InboundMessage is one row read from a session's inbound queue (runner side).
type InboundMessage struct {
	ID         int64
	Channel    string
	ChatID     string
	SenderID   string
	SenderName string
	Text       string
}

// PendingInbound returns inbound rows the container hasn't consumed yet.
func (s *SessionDBs) PendingInbound() ([]InboundMessage, error) {
	rows, err := s.Inbound.Query(`
		SELECT id, channel, chat_id, sender_id, COALESCE(sender_name, ''), text
		FROM messages
		WHERE status = 'pending'
		ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("read pending inbound: %w", err)
	}
	defer rows.Close()

	var out []InboundMessage
	for rows.Next() {
		var m InboundMessage
		if err := rows.Scan(&m.ID, &m.Channel, &m.ChatID, &m.SenderID, &m.SenderName, &m.Text); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// HasPendingInbound reports whether the session has any unconsumed inbound
// messages. The sweep uses this to decide whether a session needs its runner
// (re)launched (brief §3.3).
func (s *SessionDBs) HasPendingInbound() (bool, error) {
	var n int
	if err := s.Inbound.QueryRow(
		`SELECT count(*) FROM messages WHERE status = 'pending'`,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("count pending inbound: %w", err)
	}
	return n > 0, nil
}

// MarkInboundConsumed flips an inbound row to 'consumed' (runner side).
func (s *SessionDBs) MarkInboundConsumed(id int64) error {
	_, err := s.Inbound.Exec(
		`UPDATE messages SET status = 'consumed', consumed_at = datetime('now') WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("mark inbound consumed: %w", err)
	}
	return err
}

// EnqueueOutbound writes a reply into the session's outbound queue as 'pending'
// for the host delivery loop to pick up (runner side).
func (s *SessionDBs) EnqueueOutbound(channel, chatID, text string) (int64, error) {
	res, err := s.Outbound.Exec(`
		INSERT INTO messages (channel, chat_id, text) VALUES (?, ?, ?)`,
		channel, chatID, text,
	)
	if err != nil {
		return 0, fmt.Errorf("enqueue outbound: %w", err)
	}
	return res.LastInsertId()
}

// GetMeta returns the value for a session meta key, or ("", false) if unset.
// Stored in outbound.db, which the runner owns (multi-turn session id, etc.).
func (s *SessionDBs) GetMeta(key string) (string, bool, error) {
	var v string
	err := s.Outbound.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get meta %q: %w", key, err)
	}
	return v, true, nil
}

// SetMeta upserts a session meta key/value (runner side).
func (s *SessionDBs) SetMeta(key, value string) error {
	_, err := s.Outbound.Exec(`
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("set meta %q: %w", key, err)
	}
	return nil
}

// DeleteMeta removes a session meta key (e.g. on /reset).
func (s *SessionDBs) DeleteMeta(key string) error {
	if _, err := s.Outbound.Exec(`DELETE FROM meta WHERE key = ?`, key); err != nil {
		return fmt.Errorf("delete meta %q: %w", key, err)
	}
	return nil
}

// Close closes both session handles.
func (s *SessionDBs) Close() error {
	var firstErr error
	if s.Inbound != nil {
		if err := s.Inbound.Close(); err != nil {
			firstErr = err
		}
	}
	if s.Outbound != nil {
		if err := s.Outbound.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
