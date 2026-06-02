package db

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenSession_CreatesDirAndSchema(t *testing.T) {
	base := t.TempDir()
	s, err := OpenSession(base, 1, "telegram:555")
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer s.Close()

	// The directory and both DB files should exist.
	dir := SessionDir(base, 1, "telegram:555")
	if _, err := os.Stat(filepath.Join(dir, "inbound.db")); err != nil {
		t.Fatalf("inbound.db missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "outbound.db")); err != nil {
		t.Fatalf("outbound.db missing: %v", err)
	}

	// The messages table must exist in inbound.
	var name string
	if err := s.Inbound.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='messages'`,
	).Scan(&name); err != nil {
		t.Fatalf("inbound messages table missing: %v", err)
	}
}

func TestEnqueueInbound_WritesPendingRow(t *testing.T) {
	s, err := OpenSession(t.TempDir(), 7, "telegram:42")
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer s.Close()

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
	defer d.Close()

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
