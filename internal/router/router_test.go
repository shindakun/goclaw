package router

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/shindakun/goclaw/internal/channels"
	"github.com/shindakun/goclaw/internal/db"
)

func testDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "central.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The scenario from the field: an owner message in a fresh chat. With the owner
// seeded and auto-wire enabled, route() must create the wiring and not drop the
// message.
func TestRoute_OwnerAutoWire(t *testing.T) {
	d := testDB(t)
	_, agID, err := d.Apply(db.Bootstrap{
		OwnerTelegramID:         "6306189728",
		DefaultAgentGroupName:   "default",
		DefaultAgentGroupFolder: "default",
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	r := New(d, t.TempDir(), agID, quietLogger())
	msg := channels.InboundMsg{
		Channel:  "telegram",
		ChatID:   "555",
		SenderID: "6306189728",
		Text:     "hello",
	}
	if err := r.route(context.Background(), msg); err != nil {
		t.Fatalf("route: %v", err)
	}

	// A wiring should now exist for this conversation.
	mg, err := d.MessagingGroupByChat("telegram", "555")
	if err != nil || mg == nil {
		t.Fatalf("messaging group not recorded: %+v err=%v", mg, err)
	}
	w, err := d.WiringForMessagingGroup(mg.ID)
	if err != nil {
		t.Fatalf("wiring lookup: %v", err)
	}
	if w == nil || w.AgentGroupID != agID {
		t.Fatalf("expected auto-wiring to agent group %d, got %+v", agID, w)
	}
}

// Without auto-wire, an unknown sender in an unwired chat is simply dropped
// (recorded conversation, no wiring) — and must not error.
func TestRoute_UnknownSenderNoWiringDrops(t *testing.T) {
	d := testDB(t)
	r := New(d, t.TempDir(), 0 /* no auto-wire */, quietLogger())
	msg := channels.InboundMsg{
		Channel:  "telegram",
		ChatID:   "777",
		SenderID: "999",
		Text:     "hi",
	}
	if err := r.route(context.Background(), msg); err != nil {
		t.Fatalf("route: %v", err)
	}
	mg, err := d.MessagingGroupByChat("telegram", "777")
	if err != nil || mg == nil {
		t.Fatalf("conversation should still be recorded: %+v err=%v", mg, err)
	}
	w, _ := d.WiringForMessagingGroup(mg.ID)
	if w != nil {
		t.Fatalf("expected no wiring without auto-wire, got %+v", w)
	}
}
