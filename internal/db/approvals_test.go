package db

import "testing"

// seedAgentGroup creates an agent group and returns its id, so approval rows
// (which FK to agent_groups) have a valid parent.
func seedAgentGroup(t *testing.T, d *DB) int64 {
	t.Helper()
	id, err := d.upsertAgentGroup("default", "default")
	if err != nil {
		t.Fatalf("seed agent group: %v", err)
	}
	return id
}

func TestPendingApproval_UpsertReadDelete(t *testing.T) {
	d := openTestDB(t)
	ag := seedAgentGroup(t, d)
	p := PendingApproval{
		Channel:      "discord",
		ChatID:       "chan1",
		SenderID:     "user1",
		SenderName:   "@stranger",
		Text:         "let me in",
		AgentGroupID: ag,
	}
	id, err := d.UpsertPendingApproval(p)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero approval id")
	}

	got, err := d.PendingApprovalByID(id)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got == nil || got.Text != "let me in" || got.SenderName != "@stranger" {
		t.Fatalf("read mismatch: %+v", got)
	}

	if err := d.DeletePendingApproval(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	gone, err := d.PendingApprovalByID(id)
	if err != nil {
		t.Fatalf("read after delete: %v", err)
	}
	if gone != nil {
		t.Fatalf("expected nil after delete, got %+v", gone)
	}
}

// A repeat message from the same unknown sender refreshes the held text rather
// than creating a second row.
func TestPendingApproval_UpsertIsIdempotentPerSender(t *testing.T) {
	d := openTestDB(t)
	ag := seedAgentGroup(t, d)
	base := PendingApproval{Channel: "discord", ChatID: "c", SenderID: "u", AgentGroupID: ag}

	first := base
	first.Text = "hi"
	id1, err := d.UpsertPendingApproval(first)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	second := base
	second.Text = "still here"
	id2, err := d.UpsertPendingApproval(second)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("expected same id on repeat sender, got %d then %d", id1, id2)
	}
	got, _ := d.PendingApprovalByID(id1)
	if got.Text != "still here" {
		t.Fatalf("text not refreshed: %q", got.Text)
	}
}

func TestPendingApprovalByID_NotFound(t *testing.T) {
	d := openTestDB(t)
	got, err := d.PendingApprovalByID(99999)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for unknown id, got %+v", got)
	}
}

// Approving registers the sender as a known member, returns the original message
// for replay, and clears the pending row.
func TestApprovePendingApproval(t *testing.T) {
	d := openTestDB(t)
	ag := seedAgentGroup(t, d)
	id, err := d.UpsertPendingApproval(PendingApproval{
		Channel: "telegram", ChatID: "555", SenderID: "777", SenderName: "@newcomer",
		Text: "hello", AgentGroupID: ag,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	msg, err := d.ApprovePendingApproval(id)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if msg == nil || msg.Text != "hello" {
		t.Fatalf("approve should return the original message, got %+v", msg)
	}

	// The sender is now a known user.
	u, err := d.UserByIdentity("telegram", "777")
	if err != nil || u == nil {
		t.Fatalf("approved sender not registered: u=%v err=%v", u, err)
	}
	if u.Role != string(roleMember) {
		t.Fatalf("approved sender role = %q, want member", u.Role)
	}

	// The pending row is gone.
	gone, _ := d.PendingApprovalByID(id)
	if gone != nil {
		t.Fatal("pending row should be deleted after approve")
	}
}

// Approving an unknown id is a no-op returning (nil, nil), not an error.
func TestApprovePendingApproval_UnknownID(t *testing.T) {
	d := openTestDB(t)
	msg, err := d.ApprovePendingApproval(12345)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if msg != nil {
		t.Fatalf("expected nil for unknown id, got %+v", msg)
	}
}
