package db

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenSession_HostCreatesInboundNotOutbound(t *testing.T) {
	base := t.TempDir()
	// Host opener: owns inbound.db, only reads outbound.db. A fresh session
	// (no runner yet) has no outbound.db, and the host must NOT create it.
	s, err := OpenSession(base, 1, "telegram:555")
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() { _ = s.Close() }()

	dir := SessionDir(base, 1, "telegram:555")
	if _, err := os.Stat(filepath.Join(dir, "inbound.db")); err != nil {
		t.Fatalf("inbound.db missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "outbound.db")); !os.IsNotExist(err) {
		t.Fatalf("host must not create outbound.db; stat err = %v", err)
	}
	if s.Outbound != nil {
		t.Fatal("expected nil Outbound handle when outbound.db does not exist")
	}

	// The messages table must exist in inbound.
	var name string
	if err := s.Inbound.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='messages'`,
	).Scan(&name); err != nil {
		t.Fatalf("inbound messages table missing: %v", err)
	}

	// Outbound reads degrade gracefully to empty, not panic.
	pend, err := s.PendingOutbound()
	if err != nil {
		t.Fatalf("PendingOutbound on fresh host session: %v", err)
	}
	if len(pend) != 0 {
		t.Fatalf("expected no pending outbound, got %d", len(pend))
	}
}

func TestOpenSessionDir_RunnerCreatesBoth(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sess")
	// Runner opener: owns outbound.db, so it creates and writes it.
	s, err := OpenSessionDir(dir)
	if err != nil {
		t.Fatalf("open session dir: %v", err)
	}
	defer func() { _ = s.Close() }()
	if _, err := os.Stat(filepath.Join(dir, "outbound.db")); err != nil {
		t.Fatalf("runner should create outbound.db: %v", err)
	}
	if s.Outbound == nil {
		t.Fatal("runner Outbound handle should be non-nil")
	}
}

// TestHostCannotWriteOutbound is the enforcement guarantee for gap #1: the host
// handle opens outbound.db read-only, so a write fails at the driver - the
// single-writer-per-file invariant is enforced, not just promised (brief §5.1).
func TestHostCannotWriteOutbound(t *testing.T) {
	base := t.TempDir()
	// First let the runner create outbound.db so the host can open it read-only.
	dir := SessionDir(base, 1, "telegram:555")
	runner, err := OpenSessionDir(dir)
	if err != nil {
		t.Fatalf("runner open: %v", err)
	}
	_ = runner.Close()

	host, err := OpenSession(base, 1, "telegram:555")
	if err != nil {
		t.Fatalf("host open: %v", err)
	}
	defer func() { _ = host.Close() }()
	if host.Outbound == nil {
		t.Fatal("expected host to open existing outbound.db read-only")
	}
	// Any write to outbound.db through the host handle must be rejected.
	if _, err := host.Outbound.Exec(
		`INSERT INTO messages (channel, chat_id, text) VALUES ('x','y','z')`); err == nil {
		t.Fatal("expected read-only error writing outbound.db through the host handle")
	}
}

func TestEnqueueInbound_WritesPendingRow(t *testing.T) {
	s, err := OpenSession(t.TempDir(), 7, "telegram:42")
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() { _ = s.Close() }()

	id, err := s.EnqueueInbound("telegram", "42", "6306189728", "@steve", "hello")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero message id")
	}

	var text, status string
	if err := s.Inbound.QueryRow(
		`SELECT text, status FROM messages WHERE id = ?`, id,
	).Scan(&text, &status); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if text != "hello" || status != "pending" {
		t.Fatalf("row mismatch: text=%q status=%q", text, status)
	}
}

func TestSanitizeSessionKey(t *testing.T) {
	cases := map[string]string{
		"telegram:555": "telegram_555",
		"a/b":          "a_b",
		"..":           "_..",
		"":             "_",
		"ok-1.2_3":     "ok-1.2_3",
	}
	for in, want := range cases {
		if got := sanitizeSessionKey(in); got != want {
			t.Errorf("sanitizeSessionKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveOrCreateSession_Idempotent(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "central.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = d.Close() }()

	// A session FKs to an agent group, so create one first.
	agID, err := d.upsertAgentGroup("default", "default")
	if err != nil {
		t.Fatalf("agent group: %v", err)
	}

	s1, err := d.ResolveOrCreateSession(agID, "telegram:99")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	s2, err := d.ResolveOrCreateSession(agID, "telegram:99")
	if err != nil {
		t.Fatalf("re-resolve: %v", err)
	}
	if s1.ID != s2.ID {
		t.Fatalf("expected same session id, got %d then %d", s1.ID, s2.ID)
	}
}
