package scheduler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/shindakun/goclaw/internal/db"
	"github.com/shindakun/goclaw/internal/mounts"
)

// flakyEnsurer fails EnsureRunner until healthy is set true (models a network outage that
// later recovers). Records how many times it was called.
type flakyEnsurer struct {
	healthy bool
	calls   int
}

func (e *flakyEnsurer) EnsureRunner(_ context.Context, _ int64, _ string, _ ...mounts.Request) error {
	e.calls++
	if !e.healthy {
		return fmt.Errorf("ensure failed (outage)")
	}
	return nil
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testDB seeds a default agent group AND an owner user (the owner_user_id FK), returning
// the db and the owner's id.
func testDB(t *testing.T) (*db.DB, int64) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "central.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	ownerID, _, err := d.Apply(db.Bootstrap{
		OwnerTelegramID:         "42",
		DefaultAgentGroupName:   "default",
		DefaultAgentGroupFolder: "default",
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return d, ownerID
}

// seedTask inserts an enabled daily-at-hour task targeting a telegram session and returns
// its id.
func seedTask(t *testing.T, d *db.DB, ownerID int64, name string, atHour int, prompt string) string {
	t.Helper()
	id, err := d.CreateScheduledTask(db.ScheduledTask{
		Name:         name,
		OwnerUserID:  ownerID,
		AgentGroupID: 1,
		SessionKey:   "telegram:42",
		Channel:      "telegram",
		ChatID:       "42",
		PeriodDays:   1,
		AtHour:       atHour,
		Prompt:       prompt,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	return id
}

func atLocal(hour int) time.Time {
	return time.Date(2026, 6, 6, hour, 5, 0, 0, time.Local)
}

func TestScheduler_FiresDueTaskIntoItsTarget(t *testing.T) {
	d, owner := testDB(t)
	dir := t.TempDir()
	seedTask(t, d, owner, "inbox-summary", 8, "summarize my inbox")

	s := New(d, dir, nil, quiet())
	now := atLocal(8) // past the 08:00 boundary

	s.Tick(context.Background(), now)

	// The prompt landed in THIS task's target session.
	sess, err := db.OpenSession(dir, 1, "telegram:42")
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() { _ = sess.Close() }()
	pend, _ := sess.PendingInbound()
	if len(pend) != 1 || pend[0].Text != "summarize my inbox" {
		t.Fatalf("expected the task prompt enqueued, got %+v", pend)
	}

	// Firing again at the same time must NOT re-enqueue (last-run recorded).
	s.Tick(context.Background(), now.Add(time.Minute))
	pend2, _ := sess.PendingInbound()
	if len(pend2) != 1 {
		t.Fatalf("task double-fired: %d pending, want 1", len(pend2))
	}
}

// TestScheduler_DefersWhenRunnerCannotStart is the regression for "the morning routine
// fired during a network outage and was marked done despite doing nothing." When the runner
// cannot be ensured, fire must NOT enqueue the prompt and NOT stamp last-run, so the task
// RE-FIRES on a later tick once the outage clears.
func TestScheduler_DefersWhenRunnerCannotStart(t *testing.T) {
	d, owner := testDB(t)
	dir := t.TempDir()
	id := seedTask(t, d, owner, "inbox", 8, "summarize my inbox")

	en := &flakyEnsurer{healthy: false} // outage
	s := New(d, dir, en, quiet())
	now := atLocal(8)

	// During the outage: fires, ensure fails, so nothing is enqueued and last-run is unset.
	s.Tick(context.Background(), now)

	sess, err := db.OpenSession(dir, 1, "telegram:42")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	if pend, _ := sess.PendingInbound(); len(pend) != 0 {
		t.Fatalf("a deferred task must not enqueue its prompt; got %+v", pend)
	}
	if _, ok, _ := d.GetKV(kvPrefix + id); ok {
		t.Fatal("a deferred task must NOT stamp last-run (it would never re-fire)")
	}

	// Outage clears. A later tick (still past the boundary) must now actually fire.
	en.healthy = true
	s.Tick(context.Background(), now.Add(time.Minute))
	if pend, _ := sess.PendingInbound(); len(pend) != 1 || pend[0].Text != "summarize my inbox" {
		t.Fatalf("task did not re-fire after recovery; pending = %+v", pend)
	}
	if _, ok, _ := d.GetKV(kvPrefix + id); !ok {
		t.Fatal("last-run should be stamped once the task actually fired")
	}
}

func TestScheduler_SkipsNotYetDueAndDisabled(t *testing.T) {
	d, owner := testDB(t)
	dir := t.TempDir()
	seedTask(t, d, owner, "morning", 8, "morning task")
	// A disabled task must never fire.
	seedTask(t, d, owner, "paused", 8, "should not run")
	if _, err := d.SetScheduledTaskEnabledByName(owner, "paused", false); err != nil {
		t.Fatal(err)
	}

	s := New(d, dir, nil, quiet())
	// Before 08:00 -> the morning task is not yet due.
	s.Tick(context.Background(), atLocal(7).Add(-10*time.Minute))

	sess, _ := db.OpenSession(dir, 1, "telegram:42")
	defer func() { _ = sess.Close() }()
	if pend, _ := sess.PendingInbound(); len(pend) != 0 {
		t.Fatalf("nothing should fire before the boundary, got %+v", pend)
	}

	// After 08:00 -> only the enabled task fires (the disabled one is skipped).
	s.Tick(context.Background(), atLocal(8))
	pend, _ := sess.PendingInbound()
	if len(pend) != 1 || pend[0].Text != "morning task" {
		t.Fatalf("only the enabled task should fire, got %+v", pend)
	}
}
