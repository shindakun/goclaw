package db

import "testing"

// A runner-side session owns both DBs, so it can exercise the full boundary
// round-trip: enqueue inbound, advance the high-water mark, enqueue outbound, and
// record delivery in the ledger.
func TestSessionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSessionDir(dir)
	if err != nil {
		t.Fatalf("open session dir: %v", err)
	}
	defer func() { _ = s.Close() }()

	// --- inbound + high-water mark ---
	if hwm, err := s.InboundHWM(); err != nil || hwm != 0 {
		t.Fatalf("fresh HWM = %d, err %v; want 0", hwm, err)
	}
	id1, err := s.EnqueueInbound("discord", "c1", "u1", "@a", "first")
	if err != nil {
		t.Fatalf("enqueue inbound 1: %v", err)
	}
	id2, err := s.EnqueueInbound("discord", "c1", "u1", "@a", "second")
	if err != nil {
		t.Fatalf("enqueue inbound 2: %v", err)
	}

	if has, err := s.HasPendingInbound(); err != nil || !has {
		t.Fatalf("HasPendingInbound = %v, err %v; want true", has, err)
	}
	pend, err := s.PendingInbound()
	if err != nil {
		t.Fatalf("pending inbound: %v", err)
	}
	if len(pend) != 2 || pend[0].Text != "first" || pend[1].Text != "second" {
		t.Fatalf("pending inbound mismatch: %+v", pend)
	}

	// Advance the HWM past the first message; only the second remains pending.
	if err := s.SetInboundHWM(id1); err != nil {
		t.Fatalf("set HWM: %v", err)
	}
	if hwm, err := s.InboundHWM(); err != nil || hwm != id1 {
		t.Fatalf("HWM after set = %d, err %v; want %d", hwm, err, id1)
	}
	pend, _ = s.PendingInbound()
	if len(pend) != 1 || pend[0].ID != id2 {
		t.Fatalf("after HWM advance, pending = %+v", pend)
	}

	// Process the rest; nothing pending.
	if err := s.SetInboundHWM(id2); err != nil {
		t.Fatalf("set HWM 2: %v", err)
	}
	if has, _ := s.HasPendingInbound(); has {
		t.Fatal("expected no pending inbound after processing all")
	}

	// --- outbound + delivery ledger ---
	outID, err := s.EnqueueOutbound("discord", "c1", "reply")
	if err != nil {
		t.Fatalf("enqueue outbound: %v", err)
	}
	out, err := s.PendingOutbound()
	if err != nil {
		t.Fatalf("pending outbound: %v", err)
	}
	if len(out) != 1 || out[0].ID != outID || out[0].Text != "reply" {
		t.Fatalf("pending outbound mismatch: %+v", out)
	}

	if was, err := s.WasDelivered(outID); err != nil || was {
		t.Fatalf("WasDelivered before mark = %v, err %v; want false", was, err)
	}
	if err := s.MarkDelivered(outID); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	if was, err := s.WasDelivered(outID); err != nil || !was {
		t.Fatalf("WasDelivered after mark = %v, err %v; want true", was, err)
	}
	// Re-mark is a harmless no-op.
	if err := s.MarkDelivered(outID); err != nil {
		t.Fatalf("re-mark delivered should be a no-op: %v", err)
	}
}

func TestMarkFailed(t *testing.T) {
	s, err := OpenSessionDir(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	outID, err := s.EnqueueOutbound("telegram", "42", "boom")
	if err != nil {
		t.Fatalf("enqueue outbound: %v", err)
	}
	if err := s.MarkFailed(outID, "not authorized"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	// A failed row is in the ledger, so it counts as handled (not retried forever).
	if was, err := s.WasDelivered(outID); err != nil || !was {
		t.Fatalf("WasDelivered after MarkFailed = %v, err %v; want true (ledgered)", was, err)
	}
}

func TestSessionMeta(t *testing.T) {
	s, err := OpenSessionDir(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, ok, err := s.GetMeta("k"); err != nil || ok {
		t.Fatalf("absent meta should be (_, false, nil); ok=%v err=%v", ok, err)
	}
	if err := s.SetMeta("k", "v"); err != nil {
		t.Fatalf("set meta: %v", err)
	}
	v, ok, err := s.GetMeta("k")
	if err != nil || !ok || v != "v" {
		t.Fatalf("get meta = %q, %v, %v", v, ok, err)
	}
	// Overwrite.
	if err := s.SetMeta("k", "v2"); err != nil {
		t.Fatalf("overwrite meta: %v", err)
	}
	if v, _, _ := s.GetMeta("k"); v != "v2" {
		t.Fatalf("meta after overwrite = %q, want v2", v)
	}
	// Delete.
	if err := s.DeleteMeta("k"); err != nil {
		t.Fatalf("delete meta: %v", err)
	}
	if _, ok, _ := s.GetMeta("k"); ok {
		t.Fatal("meta should be gone after delete")
	}
}
