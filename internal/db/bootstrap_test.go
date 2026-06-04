package db

import "testing"

// TestBootstrap_OwnerMultiChannel verifies that seeding both a Telegram and a
// Discord owner id produces ONE owner user reachable from BOTH identities (not
// two separate users). This is the subtlety behind attaching a second channel
// identity to the existing owner.
func TestBootstrap_OwnerMultiChannel(t *testing.T) {
	d := openTestDB(t)

	_, _, err := d.Apply(Bootstrap{
		OwnerTelegramID:       "111111",
		OwnerDiscordID:        "222222",
		DefaultAgentGroupName: "default",
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	tg, err := d.UserByIdentity("telegram", "111111")
	if err != nil || tg == nil {
		t.Fatalf("telegram owner: %v / %v", tg, err)
	}
	dc, err := d.UserByIdentity("discord", "222222")
	if err != nil || dc == nil {
		t.Fatalf("discord owner: %v / %v", dc, err)
	}

	if tg.ID != dc.ID {
		t.Fatalf("expected ONE owner user across channels, got telegram=%d discord=%d", tg.ID, dc.ID)
	}
	if dc.Role != "owner" {
		t.Fatalf("discord identity should resolve to the owner role, got %q", dc.Role)
	}

	var users int
	if err := d.QueryRow(`SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users != 1 {
		t.Fatalf("expected exactly 1 user, got %d", users)
	}
}

// TestBootstrap_DiscordOnlyOwner: seeding only a Discord owner (no Telegram)
// still creates the owner.
func TestBootstrap_DiscordOnlyOwner(t *testing.T) {
	d := openTestDB(t)
	if _, _, err := d.Apply(Bootstrap{OwnerDiscordID: "999"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	u, err := d.UserByIdentity("discord", "999")
	if err != nil || u == nil || u.Role != "owner" {
		t.Fatalf("discord-only owner not seeded: %v / %v", u, err)
	}
}
