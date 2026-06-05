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
	"strconv"

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
		_ = sqlDB.Close()
		return nil, err
	}
	d := &DB{DB: sqlDB, path: path}
	if err := d.migrate(); err != nil {
		_ = sqlDB.Close()
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

// applyPragmas sets the journal mode, durability, foreign keys, and busy timeout
// on a fresh handle. synchronous=FULL is important for the session DBs: they are
// written by two processes (host + container) across a podman bind mount, where
// a less-durable mode can leave a committed write unflushed and lose it to a
// concurrent writer. A long busy_timeout lets a writer wait out the other's lock
// instead of failing.
func applyPragmas(sqlDB *sql.DB, journal journalMode) error {
	for _, p := range []string{
		// busy_timeout MUST be set FIRST. journal_mode (and synchronous) take a write
		// lock on the db header; without a busy_timeout already in effect, that lock
		// attempt fails IMMEDIATELY with SQLITE_BUSY if another writer holds it, instead
		// of waiting. (We hit exactly that: two host goroutines, the router enqueuing
		// inbound and the delivery loop opening inbound.db for the ledger, raced and the
		// later journal_mode = DELETE failed with "database is locked".) busy_timeout
		// itself never blocks and takes effect on the connection at once, so it leads.
		"PRAGMA busy_timeout = 10000;",
		"PRAGMA journal_mode = " + string(journal) + ";",
		"PRAGMA synchronous = FULL;",
		"PRAGMA foreign_keys = ON;",
	} {
		if _, err := sqlDB.Exec(p); err != nil {
			return fmt.Errorf("pragma %q: %w", p, err)
		}
	}
	return nil
}

// applyReadPragmas sets only the connection-level pragmas that are safe on a
// READ-ONLY handle. journal_mode and synchronous are WRITES (journal_mode mutates
// the db header), so running them on a mode=ro handle errors with "attempt to
// write a readonly database". busy_timeout is the one that matters for a reader:
// it lets a read wait out the runner's write lock instead of failing immediately.
func applyReadPragmas(sqlDB *sql.DB) error {
	for _, p := range []string{
		// busy_timeout first (see applyPragmas): so a read that meets the runner's
		// write lock waits it out instead of failing immediately.
		"PRAGMA busy_timeout = 10000;",
		"PRAGMA foreign_keys = ON;",
	} {
		if _, err := sqlDB.Exec(p); err != nil {
			return fmt.Errorf("read pragma %q: %w", p, err)
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

// OpenSession opens a session's DB pair for the HOST under the host's data dir.
// The host owns inbound.db (opened read-write) and only READS outbound.db, so
// outbound is opened READ-ONLY - the single-writer-per-file invariant (brief
// §5.1) is then enforced by the SQLite driver, not just by discipline: a stray
// host write to outbound.db fails loudly instead of silently losing across the
// podman mount (which is what caused duplicate delivery). Use OpenSessionDir for
// the runner, which owns and writes outbound.db.
func OpenSession(baseDir string, agentGroupID int64, sessionKey string) (*SessionDBs, error) {
	return OpenSessionHostDir(SessionDir(baseDir, agentGroupID, sessionKey))
}

// openInboundRW opens (creating the dir + applying the inbound schema) the
// host-owned inbound.db read-write, capped to one connection (single writer,
// brief §5.1). Shared by the host and runner open paths.
func openInboundRW(dir string) (*sql.DB, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create session dir %q: %w", dir, err)
	}
	inbound, err := sql.Open("sqlite", filepath.Join(dir, "inbound.db"))
	if err != nil {
		return nil, fmt.Errorf("open inbound.db: %w", err)
	}
	// Cap to ONE connection BEFORE applying pragmas. Connection-level pragmas
	// (busy_timeout especially) apply only to the connection they ran on; with the
	// default unbounded pool, the four pragma Execs could land on different
	// connections, so busy_timeout set on one would not protect journal_mode on
	// another. One connection makes every pragma, the schema, and all later ops share
	// the same connection state. (Single writer is also the brief §5.1 invariant.)
	inbound.SetMaxOpenConns(1)
	// Session DBs cross the container bind mount, so they must NOT use WAL -
	// use the rollback journal so cross-process writes are visible (brief §5.1).
	if err := applyPragmas(inbound, journalDelete); err != nil {
		_ = inbound.Close()
		return nil, err
	}
	if _, err := inbound.Exec(inboundSchema); err != nil {
		_ = inbound.Close()
		return nil, fmt.Errorf("init inbound schema: %w", err)
	}
	return inbound, nil
}

// OpenSessionHostDir opens a session pair for the host in an explicit directory:
// inbound.db read-write (host-owned), outbound.db READ-ONLY. If outbound.db does
// not exist yet (no runner has started for this session), Outbound is left nil
// and the outbound-reading methods degrade to "nothing there" - the host can
// enqueue inbound before any container exists. The host never creates or writes
// outbound.db; that is the runner's job (brief §5.1).
func OpenSessionHostDir(dir string) (*SessionDBs, error) {
	inbound, err := openInboundRW(dir)
	if err != nil {
		return nil, err
	}

	s := &SessionDBs{Inbound: inbound, dir: dir}

	outPath := filepath.Join(dir, "outbound.db")
	if _, statErr := os.Stat(outPath); statErr != nil {
		// No outbound.db yet: the runner hasn't created it. That's fine - leave
		// Outbound nil. We must NOT open read-only here, because read-only mode
		// cannot create the file and would error.
		return s, nil
	}
	// immutable=0, but mode=ro so the driver rejects any write. busy_timeout via
	// applyPragmas still lets reads wait out the runner's write lock.
	outbound, err := sql.Open("sqlite", "file:"+outPath+"?mode=ro")
	if err != nil {
		_ = inbound.Close()
		return nil, fmt.Errorf("open outbound.db (ro): %w", err)
	}
	// One connection, so busy_timeout (set below) applies to the connection reads
	// actually run on, not a sibling from the pool. See openInboundRW.
	outbound.SetMaxOpenConns(1)
	// Read-only handle: only set reader-safe pragmas. journal_mode/synchronous
	// are writes and error on a mode=ro db (776), which is what broke the first
	// drain after the read-only change.
	if err := applyReadPragmas(outbound); err != nil {
		_ = inbound.Close()
		_ = outbound.Close()
		return nil, err
	}
	s.Outbound = outbound
	return s, nil
}

// OpenSessionDir opens (creating + initializing schemas if needed) the
// inbound/outbound pair in an explicit directory for the RUNNER. The in-container
// agent-runner uses this against its mounted session dir, since it sees only its
// own two DBs and knows nothing of agent-group ids or the central DB (brief §3.1,
// §5.1). The runner owns outbound.db, so it is opened read-write here; the
// inbound handle is capped to one connection (single-writer, brief §5.1).
func OpenSessionDir(dir string) (*SessionDBs, error) {
	inbound, err := openInboundRW(dir)
	if err != nil {
		return nil, err
	}

	outbound, err := sql.Open("sqlite", filepath.Join(dir, "outbound.db"))
	if err != nil {
		_ = inbound.Close()
		return nil, fmt.Errorf("open outbound.db: %w", err)
	}
	if err := applyPragmas(outbound, journalDelete); err != nil {
		_ = inbound.Close()
		_ = outbound.Close()
		return nil, err
	}
	if _, err := outbound.Exec(outboundSchema); err != nil {
		_ = inbound.Close()
		_ = outbound.Close()
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
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("enqueue inbound: last id: %w", err)
	}
	// Verify the row actually persisted. The session inbound is written by both
	// the host and the container across a bind mount, where a concurrent writer
	// can clobber a committed insert; without this check we would report a write
	// that was silently lost. Read it back on the same handle.
	var got string
	err = s.Inbound.QueryRow(`SELECT text FROM messages WHERE id = ?`, id).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && got != text) {
		return 0, fmt.Errorf("enqueue inbound: write not durable (row %d missing or mismatched after insert)", id)
	}
	if err != nil {
		return 0, fmt.Errorf("enqueue inbound: verify: %w", err)
	}
	return id, nil
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
	if s.Outbound == nil {
		return nil, nil // no outbound.db yet (runner hasn't started)
	}
	rows, err := s.Outbound.Query(`
		SELECT id, channel, chat_id, text
		FROM messages
		WHERE status = 'pending'
		ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("read pending outbound: %w", err)
	}
	defer func() { _ = rows.Close() }()

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

// Delivery ledger (host side). The host records which outbound rows it has
// dispatched in the `delivered` table of inbound.db - the HOST-OWNED file - so
// the single-writer-per-file invariant holds (brief §5.1). The host never writes
// the runner's outbound.db; doing so loses writes across the podman VM mount,
// which is what made a reply get delivered twice. WasDelivered + INSERT OR IGNORE
// make delivery idempotent: even if a duplicate drain picks up the same pending
// outbound row, the second send is suppressed.

// WasDelivered reports whether the outbound row id has already been delivered or
// permanently failed - i.e. whether the host is done with it.
func (s *SessionDBs) WasDelivered(outboundID int64) (bool, error) {
	var one int
	err := s.Inbound.QueryRow(
		`SELECT 1 FROM delivered WHERE outbound_id = ?`, outboundID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check delivered: %w", err)
	}
	return true, nil
}

// MarkDelivered records that the outbound row id was delivered. INSERT OR IGNORE
// so a re-mark (e.g. an overlapping drain) is a harmless no-op.
func (s *SessionDBs) MarkDelivered(outboundID int64) error {
	_, err := s.Inbound.Exec(
		`INSERT OR IGNORE INTO delivered (outbound_id, status) VALUES (?, 'delivered')`,
		outboundID)
	if err != nil {
		return fmt.Errorf("mark delivered: %w", err)
	}
	return nil
}

// MarkFailed records that the outbound row id failed permanently (delivery not
// authorized, or the adapter Send errored). Recorded in the same ledger so the
// row is not retried forever; INSERT OR IGNORE keeps it idempotent.
func (s *SessionDBs) MarkFailed(outboundID int64, reason string) error {
	_, err := s.Inbound.Exec(
		`INSERT OR IGNORE INTO delivered (outbound_id, status, error) VALUES (?, 'failed', ?)`,
		outboundID, reason)
	if err != nil {
		return fmt.Errorf("mark failed: %w", err)
	}
	return nil
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

// metaInboundHWM is the outbound.meta key holding the highest inbound message id
// the runner has processed. The runner advances it in its OWN database
// (outbound.db) instead of writing inbound.db, so inbound.db keeps a single
// writer (the host) and a concurrent insert can't be clobbered (brief §5.1).
const metaInboundHWM = "inbound_hwm"

// InboundHWM returns the runner's processed high-water mark (0 if unset).
func (s *SessionDBs) InboundHWM() (int64, error) {
	v, ok, err := s.GetMeta(metaInboundHWM)
	if err != nil || !ok {
		return 0, err
	}
	hwm, perr := strconv.ParseInt(v, 10, 64)
	if perr != nil {
		return 0, fmt.Errorf("parse inbound hwm %q: %w", v, perr)
	}
	return hwm, nil
}

// SetInboundHWM records the runner's processed high-water mark (runner side,
// written to outbound.db which the runner owns).
func (s *SessionDBs) SetInboundHWM(id int64) error {
	return s.SetMeta(metaInboundHWM, strconv.FormatInt(id, 10))
}

// PendingInbound returns inbound rows the runner hasn't processed yet, i.e. with
// id greater than the runner's high-water mark. Reads only; the runner never
// writes inbound.db.
func (s *SessionDBs) PendingInbound() ([]InboundMessage, error) {
	hwm, err := s.InboundHWM()
	if err != nil {
		return nil, err
	}
	rows, err := s.Inbound.Query(`
		SELECT id, channel, chat_id, sender_id, COALESCE(sender_name, ''), text
		FROM messages
		WHERE id > ?
		ORDER BY id ASC`, hwm)
	if err != nil {
		return nil, fmt.Errorf("read pending inbound: %w", err)
	}
	defer func() { _ = rows.Close() }()

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

// HasPendingInbound reports whether any inbound message is past the runner's
// high-water mark. The sweep uses this to decide whether a session needs its
// runner (re)launched (brief §3.3).
func (s *SessionDBs) HasPendingInbound() (bool, error) {
	hwm, err := s.InboundHWM()
	if err != nil {
		return false, err
	}
	var n int
	if err := s.Inbound.QueryRow(
		`SELECT count(*) FROM messages WHERE id > ?`, hwm,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("count pending inbound: %w", err)
	}
	return n > 0, nil
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
	if s.Outbound == nil {
		return "", false, nil // no outbound.db yet (runner hasn't started)
	}
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
