// Package db owns the central goclaw database and the per-session
// inbound/outbound databases. It uses the pure-Go modernc.org/sqlite driver so
// the host keeps its single static-binary story (brief §5). WAL mode is enabled
// on every handle, and writer handles are capped to a single connection to
// preserve the single-writer-per-DB invariant (brief §5.1).
package db

import (
	"database/sql"
	_ "embed"
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
	if err := applyPragmas(sqlDB); err != nil {
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

// applyPragmas sets WAL + foreign keys on a fresh handle.
func applyPragmas(sqlDB *sql.DB) error {
	for _, p := range []string{
		"PRAGMA journal_mode = WAL;",
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

// SessionDir returns the on-disk directory for a session's DB pair:
// {baseDir}/v2-sessions/{agentGroupID}/{sessionKey}/ (brief §3.2). The session
// key derives from external input (a chat id), so it is sanitized to a safe
// single path segment — no separators or traversal can escape the base dir.
func SessionDir(baseDir string, agentGroupID int64, sessionKey string) string {
	return filepath.Join(baseDir, "v2-sessions", fmt.Sprintf("%d", agentGroupID), sanitizeSessionKey(sessionKey))
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

// OpenSession creates the session directory if needed, opens the inbound/outbound
// pair, and ensures both message-queue schemas exist. The inbound handle is
// capped to one connection (host is its sole writer, brief §5.1).
func OpenSession(baseDir string, agentGroupID int64, sessionKey string) (*SessionDBs, error) {
	dir := SessionDir(baseDir, agentGroupID, sessionKey)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create session dir %q: %w", dir, err)
	}

	inbound, err := sql.Open("sqlite", filepath.Join(dir, "inbound.db"))
	if err != nil {
		return nil, fmt.Errorf("open inbound.db: %w", err)
	}
	if err := applyPragmas(inbound); err != nil {
		inbound.Close()
		return nil, err
	}
	// Host is the sole writer of inbound — enforce one connection.
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
	if err := applyPragmas(outbound); err != nil {
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
