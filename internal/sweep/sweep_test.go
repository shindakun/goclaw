package sweep

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/shindakun/goclaw/internal/db"
)

type fakeEnsurer struct {
	calls []string // sessionKeys ensured
}

func (f *fakeEnsurer) EnsureRunner(ctx context.Context, agentGroupID int64, sessionKey, sessionDir string) error {
	f.calls = append(f.calls, sessionKey)
	return nil
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func setup(t *testing.T) (*db.DB, int64, string) {
	t.Helper()
	dataDir := t.TempDir()
	central, err := db.Open(filepath.Join(dataDir, "central.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { central.Close() })
	_, agID, err := central.Apply(db.Bootstrap{
		DefaultAgentGroupName:   "default",
		DefaultAgentGroupFolder: "default",
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return central, agID, dataDir
}

// A session with pending inbound triggers EnsureRunner.
func TestRecoverRunners_EnsuresWhenPending(t *testing.T) {
	central, agID, dataDir := setup(t)
	const key = "telegram:555"
	if _, err := central.ResolveOrCreateSession(agID, key); err != nil {
		t.Fatalf("session: %v", err)
	}
	sess, err := db.OpenSession(dataDir, agID, key)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	if _, err := sess.EnqueueInbound("telegram", "555", "u", "n", "queued while runner dead"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	sess.Close()

	fe := &fakeEnsurer{}
	s := New(central, dataDir, fe, quiet())
	s.recoverRunners(context.Background())

	if len(fe.calls) != 1 || fe.calls[0] != key {
		t.Fatalf("expected EnsureRunner for %q, got %v", key, fe.calls)
	}
}

// A session with no pending inbound is left alone (don't spin up idle runners).
func TestRecoverRunners_SkipsWhenNoPending(t *testing.T) {
	central, agID, dataDir := setup(t)
	const key = "telegram:777"
	if _, err := central.ResolveOrCreateSession(agID, key); err != nil {
		t.Fatalf("session: %v", err)
	}
	// Open the session (creates empty queues) but enqueue nothing.
	sess, err := db.OpenSession(dataDir, agID, key)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	sess.Close()

	fe := &fakeEnsurer{}
	s := New(central, dataDir, fe, quiet())
	s.recoverRunners(context.Background())

	if len(fe.calls) != 0 {
		t.Fatalf("expected no EnsureRunner calls, got %v", fe.calls)
	}
}

// A nil ensurer (no orchestration) is a no-op, not a panic.
func TestRecoverRunners_NilEnsurerNoop(t *testing.T) {
	central, _, dataDir := setup(t)
	s := New(central, dataDir, nil, quiet())
	s.recoverRunners(context.Background()) // must not panic
}
