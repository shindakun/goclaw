package delivery

import (
	"context"
	"fmt"
	"testing"

	"github.com/shindakun/goclaw/internal/db"
)

// TestRoundTrip simulates the whole boundary on disk without a container:
//
//	host enqueues inbound  →  (stub runner) consumes + echoes to outbound  →
//	delivery dispatches the echo via the adapter.
//
// This is the Phase 0 proof that the two-SQLite-file boundary round-trips.
func TestRoundTrip(t *testing.T) {
	d, central, agID, dataDir, fake := setup(t)
	const key = "telegram:555"

	if _, err := central.ResolveOrCreateSession(agID, key); err != nil {
		t.Fatalf("session row: %v", err)
	}

	// 1. Host writes an inbound message (what router.enqueue does).
	sess, err := db.OpenSession(dataDir, agID, key)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	if _, err := sess.EnqueueInbound("telegram", "555", "6306189728", "@steve", "ping"); err != nil {
		t.Fatalf("enqueue inbound: %v", err)
	}

	// 2. Stub runner: consume pending inbound, echo to outbound.
	pending, err := sess.PendingInbound()
	if err != nil {
		t.Fatalf("pending inbound: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending inbound, got %d", len(pending))
	}
	for _, m := range pending {
		if _, err := sess.EnqueueOutbound(m.Channel, m.ChatID, fmt.Sprintf("echo: %s", m.Text)); err != nil {
			t.Fatalf("enqueue outbound: %v", err)
		}
		if err := sess.SetInboundHWM(m.ID); err != nil {
			t.Fatalf("advance hwm: %v", err)
		}
	}
	sess.Close()

	// 3. Delivery dispatches the echo.
	if err := d.drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if len(fake.sent) != 1 || fake.sent[0].Text != "echo: ping" || fake.sent[0].ChatID != "555" {
		t.Fatalf("round-trip failed; delivered: %+v", fake.sent)
	}
}
