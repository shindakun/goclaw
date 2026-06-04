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

	// 1. Host writes an inbound message (what router.enqueue does). Host opener:
	// inbound read-write, outbound read-only.
	host, err := db.OpenSession(dataDir, agID, key)
	if err != nil {
		t.Fatalf("open session (host): %v", err)
	}
	if _, err := host.EnqueueInbound("telegram", "555", "6306189728", "@steve", "ping"); err != nil {
		t.Fatalf("enqueue inbound: %v", err)
	}
	_ = host.Close()

	// 2. Stub runner: consume pending inbound, echo to outbound. Runner opener:
	// outbound read-write (it owns outbound.db + the inbound high-water mark).
	runner, err := db.OpenSessionDir(db.SessionDir(dataDir, agID, key))
	if err != nil {
		t.Fatalf("open session (runner): %v", err)
	}
	pending, err := runner.PendingInbound()
	if err != nil {
		t.Fatalf("pending inbound: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending inbound, got %d", len(pending))
	}
	for _, m := range pending {
		if _, err := runner.EnqueueOutbound(m.Channel, m.ChatID, fmt.Sprintf("echo: %s", m.Text)); err != nil {
			t.Fatalf("enqueue outbound: %v", err)
		}
		if err := runner.SetInboundHWM(m.ID); err != nil {
			t.Fatalf("advance hwm: %v", err)
		}
	}
	_ = runner.Close()

	// 3. Delivery dispatches the echo.
	if err := d.drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if len(fake.sent) != 1 || fake.sent[0].Text != "echo: ping" || fake.sent[0].ChatID != "555" {
		t.Fatalf("round-trip failed; delivered: %+v", fake.sent)
	}
}
