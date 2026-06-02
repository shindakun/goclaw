package delivery

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/shindakun/goclaw/internal/channels"
	"github.com/shindakun/goclaw/internal/db"
)

// fakeAdapter records what was sent and never touches a network.
type fakeAdapter struct {
	mu   sync.Mutex
	sent []channels.OutboundMsg
}

func (f *fakeAdapter) Name() string { return "telegram" }
func (f *fakeAdapter) Start(ctx context.Context) (<-chan channels.InboundMsg, error) {
	ch := make(chan channels.InboundMsg)
	close(ch)
	return ch, nil
}
func (f *fakeAdapter) Send(ctx context.Context, out channels.OutboundMsg) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, out)
	return nil
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// setup builds a central DB with one agent group + session, returns the
// deliverer, the open session, the agent group id, and the data dir.
func setup(t *testing.T) (*Deliverer, *db.DB, int64, string, *fakeAdapter) {
	t.Helper()
	dataDir := t.TempDir()
	central, err := db.Open(filepath.Join(dataDir, "central.db"))
	if err != nil {
		t.Fatalf("open central: %v", err)
	}
	t.Cleanup(func() { central.Close() })

	_, agID, err := central.Apply(db.Bootstrap{
		DefaultAgentGroupName:   "default",
		DefaultAgentGroupFolder: "default",
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	fake := &fakeAdapter{}
	reg := channels.NewRegistry()
	if err := reg.Register(fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	d := New(central, reg, dataDir, quiet())
	return d, central, agID, dataDir, fake
}

func TestDrain_DeliversOriginChat(t *testing.T) {
	d, central, agID, dataDir, fake := setup(t)

	// Session for telegram:555; the agent writes a reply to the origin chat.
	const key = "telegram:555"
	if _, err := central.ResolveOrCreateSession(agID, key); err != nil {
		t.Fatalf("session row: %v", err)
	}
	sess, err := db.OpenSession(dataDir, agID, key)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	if _, err := sess.EnqueueOutbound("telegram", "555", "echo: hi"); err != nil {
		t.Fatalf("enqueue outbound: %v", err)
	}
	sess.Close()

	if err := d.drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if len(fake.sent) != 1 || fake.sent[0].Text != "echo: hi" {
		t.Fatalf("expected one delivered echo, got %+v", fake.sent)
	}

	// Row must be marked delivered (no longer pending).
	sess, _ = db.OpenSession(dataDir, agID, key)
	defer sess.Close()
	pend, _ := sess.PendingOutbound()
	if len(pend) != 0 {
		t.Fatalf("expected no pending after delivery, got %d", len(pend))
	}
}

func TestDrain_DeniesNonOriginWithoutDestination(t *testing.T) {
	d, central, agID, dataDir, fake := setup(t)

	const key = "telegram:555"
	if _, err := central.ResolveOrCreateSession(agID, key); err != nil {
		t.Fatalf("session row: %v", err)
	}
	sess, err := db.OpenSession(dataDir, agID, key)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	// Reply targets a DIFFERENT chat with no agent_destinations row → denied.
	if _, err := sess.EnqueueOutbound("telegram", "999", "leak"); err != nil {
		t.Fatalf("enqueue outbound: %v", err)
	}
	sess.Close()

	if err := d.drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(fake.sent) != 0 {
		t.Fatalf("expected nothing delivered, got %+v", fake.sent)
	}
}
