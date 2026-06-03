package router

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/shindakun/goclaw/internal/channels"
	"github.com/shindakun/goclaw/internal/db"
	"github.com/shindakun/goclaw/internal/permissions"
)

// fakeSender records host-originated messages.
type fakeSender struct {
	sent []channels.OutboundMsg
}

func (f *fakeSender) Send(ctx context.Context, out channels.OutboundMsg) error {
	f.sent = append(f.sent, out)
	return nil
}

// approvalSetup seeds an owner, a default agent group, and a wiring with the
// request_approval policy on the unknown chat. Returns the router, db, sender,
// and the agent group id.
func approvalSetup(t *testing.T) (*Router, *db.DB, *fakeSender, int64) {
	t.Helper()
	d := testDB(t)
	ownerID, agID, err := d.Apply(db.Bootstrap{
		OwnerTelegramID:         "1000", // owner's telegram id
		DefaultAgentGroupName:   "default",
		DefaultAgentGroupFolder: "default",
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if ownerID == 0 {
		t.Fatal("owner not seeded")
	}
	// Wire the unknown sender's chat to the group with request_approval.
	mgID, err := d.UpsertMessagingGroup("telegram", "555", "")
	if err != nil {
		t.Fatalf("mg: %v", err)
	}
	if _, err := d.EnsureWiring(mgID, agID, string(permissions.ScopeAll), string(permissions.PolicyRequestApproval)); err != nil {
		t.Fatalf("wiring: %v", err)
	}
	fs := &fakeSender{}
	r := New(d, t.TempDir(), 0, nil, fs, nil, quietLogger())
	return r, d, fs, agID
}

func TestApproval_UnknownSenderHeldAndCardSent(t *testing.T) {
	r, d, fs, agID := approvalSetup(t)

	// Unknown sender messages the request_approval chat.
	msg := channels.InboundMsg{
		Channel: "telegram", ChatID: "555", SenderID: "777", Sender: "@stranger", Text: "let me in",
	}
	if err := r.route(context.Background(), msg); err != nil {
		t.Fatalf("route: %v", err)
	}

	// A pending approval row must exist.
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM pending_approvals WHERE sender_id='777' AND agent_group_id=?`, agID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 pending approval, got %d", n)
	}
	// An approval card must have gone to the owner (telegram chat "1000").
	if len(fs.sent) != 1 {
		t.Fatalf("expected 1 approval card, got %d", len(fs.sent))
	}
	if fs.sent[0].ChatID != "1000" || !strings.Contains(fs.sent[0].Text, "/approve") {
		t.Fatalf("approval card not addressed to owner with command: %+v", fs.sent[0])
	}
}

func TestApproval_OwnerApprovesAndMessageReplays(t *testing.T) {
	r, d, _, agID := approvalSetup(t)

	// 1. Stranger requests access.
	if err := r.route(context.Background(), channels.InboundMsg{
		Channel: "telegram", ChatID: "555", SenderID: "777", Sender: "@stranger", Text: "let me in",
	}); err != nil {
		t.Fatalf("route stranger: %v", err)
	}
	var approvalID int64
	if err := d.QueryRow(`SELECT id FROM pending_approvals WHERE sender_id='777'`).Scan(&approvalID); err != nil {
		t.Fatalf("get approval id: %v", err)
	}

	// 2. Owner approves.
	if err := r.route(context.Background(), channels.InboundMsg{
		Channel: "telegram", ChatID: "1000", SenderID: "1000", Sender: "owner",
		Text: "/approve " + itoa(approvalID),
	}); err != nil {
		t.Fatalf("route approve: %v", err)
	}

	// The stranger is now a known member.
	u, err := d.UserByIdentity("telegram", "777")
	if err != nil || u == nil {
		t.Fatalf("stranger not registered as member: %+v err=%v", u, err)
	}
	// The pending row is gone.
	var n int
	d.QueryRow(`SELECT count(*) FROM pending_approvals WHERE id=?`, approvalID).Scan(&n)
	if n != 0 {
		t.Fatalf("pending approval not cleared")
	}
	// The replayed message landed in the session inbound (group dir, "telegram:555").
	sess, err := db.OpenSession(r.dataDir, agID, "telegram:555")
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer sess.Close()
	in, _ := sess.PendingInbound()
	if len(in) != 1 || in[0].Text != "let me in" {
		t.Fatalf("approved message did not replay into inbound: %+v", in)
	}
}

func TestApproval_OwnerDeniesClearsRow(t *testing.T) {
	r, d, _, _ := approvalSetup(t)
	if err := r.route(context.Background(), channels.InboundMsg{
		Channel: "telegram", ChatID: "555", SenderID: "777", Text: "let me in",
	}); err != nil {
		t.Fatalf("route: %v", err)
	}
	var id int64
	d.QueryRow(`SELECT id FROM pending_approvals WHERE sender_id='777'`).Scan(&id)

	if err := r.route(context.Background(), channels.InboundMsg{
		Channel: "telegram", ChatID: "1000", SenderID: "1000", Text: "/deny " + itoa(id),
	}); err != nil {
		t.Fatalf("route deny: %v", err)
	}
	var n int
	d.QueryRow(`SELECT count(*) FROM pending_approvals WHERE id=?`, id).Scan(&n)
	if n != 0 {
		t.Fatalf("deny did not clear the pending row")
	}
	// Stranger must NOT have become a member.
	if u, _ := d.UserByIdentity("telegram", "777"); u != nil {
		t.Fatalf("denied stranger should not be a member")
	}
}

// A non-owner issuing /approve is ignored (falls through to normal routing).
func TestApproval_NonOwnerCommandIgnored(t *testing.T) {
	r, d, _, _ := approvalSetup(t)
	// Stranger tries to approve themselves.
	handled, err := r.handleApprovalCommand(context.Background(),
		channels.InboundMsg{Channel: "telegram", ChatID: "555", SenderID: "777", Text: "/approve 1"},
		nil /* unknown user */)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if handled {
		t.Fatal("non-owner /approve should not be handled")
	}
	_ = d
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
