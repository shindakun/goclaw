package db

import (
	"errors"
	"path/filepath"
	"testing"
)

func taskTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "central.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, _, err := d.Apply(Bootstrap{OwnerTelegramID: "1", DefaultAgentGroupName: "default", DefaultAgentGroupFolder: "default"}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return d
}

func mkTask(owner int64, name string) ScheduledTask {
	return ScheduledTask{
		Name: name, OwnerUserID: owner, AgentGroupID: 1,
		SessionKey: "telegram:1", Channel: "telegram", ChatID: "1",
		PeriodDays: 1, AtHour: 8, Prompt: "do it", Enabled: true,
	}
}

func TestScheduledTask_CRUD(t *testing.T) {
	d := taskTestDB(t)

	id, err := d.CreateScheduledTask(mkTask(1, "inbox"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == "" {
		t.Fatal("empty id")
	}

	// Listed for the owner, and present in the enabled set the scheduler reads.
	mine, err := d.ScheduledTasksByOwner(1)
	if err != nil || len(mine) != 1 || mine[0].Name != "inbox" {
		t.Fatalf("ScheduledTasksByOwner = %+v, %v", mine, err)
	}
	en, _ := d.EnabledScheduledTasks()
	if len(en) != 1 {
		t.Fatalf("EnabledScheduledTasks = %d, want 1", len(en))
	}

	// Disable -> still listed for owner, but NOT in the enabled set.
	if ok, _ := d.SetScheduledTaskEnabledByName(1, "inbox", false); !ok {
		t.Fatal("disable reported no match")
	}
	if en, _ := d.EnabledScheduledTasks(); len(en) != 0 {
		t.Fatalf("disabled task still enabled: %d", len(en))
	}
	if mine, _ := d.ScheduledTasksByOwner(1); len(mine) != 1 || mine[0].Enabled {
		t.Fatalf("disabled task should still be listed (paused): %+v", mine)
	}

	// Delete -> gone.
	if ok, _ := d.DeleteScheduledTaskByName(1, "inbox"); !ok {
		t.Fatal("delete reported no match")
	}
	if mine, _ := d.ScheduledTasksByOwner(1); len(mine) != 0 {
		t.Fatalf("task not deleted: %+v", mine)
	}
}

func TestScheduledTask_DuplicateNamePerOwner(t *testing.T) {
	d := taskTestDB(t)
	if _, err := d.CreateScheduledTask(mkTask(1, "dup")); err != nil {
		t.Fatal(err)
	}
	_, err := d.CreateScheduledTask(mkTask(1, "dup"))
	if !errors.Is(err, ErrTaskExists) {
		t.Fatalf("second create with same owner+name = %v, want ErrTaskExists", err)
	}
}

// Owner isolation: one user cannot see, delete, or disable another user's task.
func TestScheduledTask_OwnerIsolation(t *testing.T) {
	d := taskTestDB(t)
	// A second user.
	other, err := d.UpsertUserWithIdentity("other", "member", "telegram", "999")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateScheduledTask(mkTask(1, "secret")); err != nil {
		t.Fatal(err)
	}

	// Other owner sees nothing.
	if mine, _ := d.ScheduledTasksByOwner(other); len(mine) != 0 {
		t.Fatalf("other user sees owner's task: %+v", mine)
	}
	// Other owner cannot delete or disable it (no row matches their owner id).
	if ok, _ := d.DeleteScheduledTaskByName(other, "secret"); ok {
		t.Fatal("other user deleted owner's task")
	}
	if ok, _ := d.SetScheduledTaskEnabledByName(other, "secret", false); ok {
		t.Fatal("other user disabled owner's task")
	}
	// The owner's task is intact and still enabled.
	if en, _ := d.EnabledScheduledTasks(); len(en) != 1 {
		t.Fatalf("owner's task was tampered with cross-owner: %d enabled", len(en))
	}

	// Same name under different owners is allowed (unique is per-owner).
	if _, err := d.CreateScheduledTask(mkTask(other, "secret")); err != nil {
		t.Fatalf("same name for a different owner should be allowed: %v", err)
	}
	if c, _ := d.CountScheduledTasksByOwner(other); c != 1 {
		t.Fatalf("other owner count = %d, want 1", c)
	}
}
