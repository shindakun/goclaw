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
	t.Cleanup(func() { _ = d.Close() })
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

	dataDir := t.TempDir()
	r := New(d, dataDir, agID, nil /* no runner launch */, nil /* no sender */, nil /* no typer */, nil /* no command registry */, quietLogger())
	msg := channels.InboundMsg{
		Channel:  "telegram",
		ChatID:   "555",
		SenderID: "6306189728",
		Sender:   "@steve",
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

	// And the message must have landed in the session's inbound.db as pending.
	sess, err := db.OpenSession(dataDir, agID, "telegram:555")
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() { _ = sess.Close() }()
	var text, status string
	if err := sess.Inbound.QueryRow(
		`SELECT text, status FROM messages ORDER BY id DESC LIMIT 1`,
	).Scan(&text, &status); err != nil {
		t.Fatalf("inbound row not found: %v", err)
	}
	if text != "hello" || status != "pending" {
		t.Fatalf("inbound row mismatch: text=%q status=%q", text, status)
	}
}

// Without auto-wire, an unknown sender in an unwired chat is simply dropped
// (recorded conversation, no wiring) - and must not error.
func TestRoute_UnknownSenderNoWiringDrops(t *testing.T) {
	d := testDB(t)
	r := New(d, t.TempDir(), 0 /* no auto-wire */, nil /* no runner launch */, nil /* no sender */, nil /* no typer */, nil /* no command registry */, quietLogger())
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
