// Package db owns the central goclaw database and the per-session
// inbound/outbound databases. It uses the pure-Go modernc.org/sqlite driver so
// the host keeps its single static-binary story (brief §5). WAL mode is enabled
// on every handle, and writer handles are capped to a single connection to
// preserve the single-writer-per-DB invariant (brief §5.1).
package db

import (
	"database/sql"
	"fmt"
	"path/filepath"

	_ "modernc.org/sqlite"
)

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

// OpenSession opens the inbound/outbound pair under
// data/v2-sessions/{agentGroupID}/{sessionKey}/.
//
// TODO: create the directory, ensure the inbound/outbound schemas exist (the
// message-queue tables), and mount these two files into the container.
func OpenSession(baseDir string, agentGroupID int64, sessionKey string) (*SessionDBs, error) {
	dir := filepath.Join(baseDir, "v2-sessions", fmt.Sprintf("%d", agentGroupID), sessionKey)

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

	return &SessionDBs{Inbound: inbound, Outbound: outbound, dir: dir}, nil
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
