package sweep

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/shindakun/goclaw/internal/db"
	"github.com/shindakun/goclaw/internal/mounts"
)

// fakeRunners is a stand-in RunnerManager that records calls and lets a test
// script the currently-running runner set.
type fakeRunners struct {
	calls   []int64 // agentGroupIDs ensured
	running []int64 // ids reported by RunningGroupIDs
	stopped []int64 // agentGroupIDs stopped
}

func (f *fakeRunners) EnsureRunner(ctx context.Context, agentGroupID int64, groupDir string, extra ...mounts.Request) error {
	f.calls = append(f.calls, agentGroupID)
	return nil
}

func (f *fakeRunners) RunningGroupIDs(ctx context.Context) ([]int64, error) {
	return f.running, nil
}

func (f *fakeRunners) StopGroupRunner(ctx context.Context, agentGroupID int64) error {
	f.stopped = append(f.stopped, agentGroupID)
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

// A session with pending inbound triggers EnsureRunner for its agent group.
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

	fe := &fakeRunners{}
	s := New(central, dataDir, fe, quiet())
	s.recoverRunners(context.Background())

	if len(fe.calls) != 1 || fe.calls[0] != agID {
		t.Fatalf("expected EnsureRunner for agent group %d, got %v", agID, fe.calls)
	}
}

// Two sessions in the SAME agent group → only one EnsureRunner call (one
// container per group).
func TestRecoverRunners_DedupesByAgentGroup(t *testing.T) {
	central, agID, dataDir := setup(t)
	for _, key := range []string{"telegram:111", "telegram:222"} {
		if _, err := central.ResolveOrCreateSession(agID, key); err != nil {
			t.Fatalf("session: %v", err)
		}
		sess, err := db.OpenSession(dataDir, agID, key)
		if err != nil {
			t.Fatalf("open session: %v", err)
		}
		if _, err := sess.EnqueueInbound("telegram", key, "u", "n", "hi"); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		sess.Close()
	}

	fe := &fakeRunners{}
	s := New(central, dataDir, fe, quiet())
	s.recoverRunners(context.Background())

	if len(fe.calls) != 1 || fe.calls[0] != agID {
		t.Fatalf("expected a single EnsureRunner for agent group %d, got %v", agID, fe.calls)
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

	fe := &fakeRunners{}
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
	s.gcIdleRunners(context.Background(), time.Now())
}

// An idle group (no recent activity, no pending inbound) gets its runner reaped.
func TestGCIdleRunners_ReapsIdle(t *testing.T) {
	central, agID, dataDir := setup(t)
	const key = "telegram:555"
	if _, err := central.ResolveOrCreateSession(agID, key); err != nil {
		t.Fatalf("session: %v", err)
	}
	// Session exists with no pending inbound. Make it "idle" by GC-ing with a
	// now far enough in the future that last_active_at < now-idleTTL.
	fr := &fakeRunners{running: []int64{agID}}
	s := New(central, dataDir, fr, quiet())
	s.gcIdleRunners(context.Background(), time.Now().Add(24*time.Hour))

	if len(fr.stopped) != 1 || fr.stopped[0] != agID {
		t.Fatalf("expected group %d reaped, got %v", agID, fr.stopped)
	}
}

// A recently-active group is NOT reaped.
func TestGCIdleRunners_KeepsRecentlyActive(t *testing.T) {
	central, agID, dataDir := setup(t)
	const key = "telegram:555"
	if _, err := central.ResolveOrCreateSession(agID, key); err != nil {
		t.Fatalf("session: %v", err)
	}
	fr := &fakeRunners{running: []int64{agID}}
	s := New(central, dataDir, fr, quiet())
	// now is "real" now, so the just-stamped last_active_at is within idleTTL.
	s.gcIdleRunners(context.Background(), time.Now())

	if len(fr.stopped) != 0 {
		t.Fatalf("expected no reaping of an active group, got %v", fr.stopped)
	}
}

// A group with pending inbound is kept even if its activity timestamp is old.
func TestGCIdleRunners_KeepsGroupWithPending(t *testing.T) {
	central, agID, dataDir := setup(t)
	const key = "telegram:555"
	if _, err := central.ResolveOrCreateSession(agID, key); err != nil {
		t.Fatalf("session: %v", err)
	}
	sess, err := db.OpenSession(dataDir, agID, key)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	if _, err := sess.EnqueueInbound("telegram", "555", "u", "n", "still queued"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	sess.Close()

	fr := &fakeRunners{running: []int64{agID}}
	s := New(central, dataDir, fr, quiet())
	s.gcIdleRunners(context.Background(), time.Now().Add(24*time.Hour)) // "idle" by time

	if len(fr.stopped) != 0 {
		t.Fatalf("expected group with pending inbound to be kept, got %v", fr.stopped)
	}
}
