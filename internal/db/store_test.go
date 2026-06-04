package db

import (
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "central.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestUserByIdentity_NotFoundReturnsNil(t *testing.T) {
	d := openTestDB(t)
	u, err := d.UserByIdentity("telegram", "999")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if u != nil {
		t.Fatalf("expected nil user, got %+v", u)
	}
}

func TestUpsertUserWithIdentity_SeedsAndResolves(t *testing.T) {
	d := openTestDB(t)

	id, err := d.UpsertUserWithIdentity("owner", "owner", "telegram", "6306189728")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero user id")
	}

	u, err := d.UserByIdentity("telegram", "6306189728")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if u == nil || u.Role != "owner" || u.ID != id {
		t.Fatalf("resolved user mismatch: %+v", u)
	}

	// Idempotent: re-seeding the same identity updates, not duplicates.
	id2, err := d.UpsertUserWithIdentity("owner", "admin", "telegram", "6306189728")
	if err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if id2 != id {
		t.Fatalf("expected same user id %d, got %d", id, id2)
	}
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 user after re-seed, got %d", count)
	}
}

func TestUpsertMessagingGroupAndWiring(t *testing.T) {
	d := openTestDB(t)

	mgID, err := d.UpsertMessagingGroup("telegram", "12345", "Test Chat")
	if err != nil {
		t.Fatalf("upsert mg: %v", err)
	}

	// No wiring yet.
	w, err := d.WiringForMessagingGroup(mgID)
	if err != nil {
		t.Fatalf("wiring lookup: %v", err)
	}
	if w != nil {
		t.Fatalf("expected no wiring, got %+v", w)
	}

	agID, err := d.upsertAgentGroup("default", "default")
	if err != nil {
		t.Fatalf("agent group: %v", err)
	}
	if _, err := d.EnsureWiring(mgID, agID, "owner", "strict"); err != nil {
		t.Fatalf("ensure wiring: %v", err)
	}

	w, err = d.WiringForMessagingGroup(mgID)
	if err != nil {
		t.Fatalf("wiring lookup 2: %v", err)
	}
	if w == nil || w.AgentGroupID != agID || w.SenderScope != "owner" || w.UnknownSenderPolicy != "strict" {
		t.Fatalf("wiring mismatch: %+v", w)
	}
}

func TestApply_Bootstrap(t *testing.T) {
	d := openTestDB(t)
	ownerID, agID, err := d.Apply(Bootstrap{
		OwnerTelegramID:         "6306189728",
		DefaultAgentGroupName:   "default",
		DefaultAgentGroupFolder: "default",
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if ownerID == 0 || agID == 0 {
		t.Fatalf("expected non-zero ids, got owner=%d agentGroup=%d", ownerID, agID)
	}
	u, err := d.UserByIdentity("telegram", "6306189728")
	if err != nil || u == nil || u.Role != "owner" {
		t.Fatalf("owner not seeded correctly: %+v err=%v", u, err)
	}
}
