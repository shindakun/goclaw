package maintenance

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

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

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newSched builds a scheduler for the due() tests, which never open session DBs,
// so the data dir is irrelevant there.
func newSched(d *db.DB) *Scheduler {
	target := Target{AgentGroupID: 1, SessionKey: "telegram:1", Channel: "telegram", ChatID: "1"}
	return New(d, "", nil, target, quiet())
}

func at(hour int) time.Time {
	// A fixed date at the given local hour.
	return time.Date(2026, 6, 3, hour, 0, 0, 0, time.Local)
}

func TestDue_FirstRunAtOrAfterHour(t *testing.T) {
	s := newSched(testDB(t))
	job := Job{Name: "nightly", PeriodDays: 1, AtHour: 22}

	// Before the hour: not due.
	due, err := s.due(job, at(21))
	if err != nil || due {
		t.Fatalf("expected not due before AtHour, got due=%v err=%v", due, err)
	}
	// At/after the hour, never run: due.
	due, err = s.due(job, at(22))
	if err != nil || !due {
		t.Fatalf("expected due at AtHour on first run, got due=%v err=%v", due, err)
	}
}

// atDay returns a fixed local time on a given day-of-June-2026 at an hour.
func atDay(day, hour int) time.Time {
	return time.Date(2026, 6, day, hour, 0, 0, 0, time.Local)
}

// TestDue_DailyBoundary_NoDrift is the bug the user hit: morning ran late the
// previous night, then the host starts mid-morning the NEXT day. Under the old
// 20h-interval rule it stayed "not due" until evening (drift). Pinned to the
// 07:00 local boundary, it must be due when the host starts at 09:00 the next day.
func TestDue_DailyBoundary_NoDrift(t *testing.T) {
	d := testDB(t)
	s := newSched(d)
	job := Job{Name: "morning", PeriodDays: 1, AtHour: 7}

	// Ran 2026-06-02 at 21:52 (last night, late).
	lastNight := atDay(2, 21).Add(52 * time.Minute)
	if err := d.SetKV(kvPrefix+job.Name, lastNight.Format(time.RFC3339)); err != nil {
		t.Fatalf("setkv: %v", err)
	}
	// Host starts 2026-06-03 at 09:00 - past today's 07:00 boundary, not run since it.
	if due, err := s.due(job, atDay(3, 9)); err != nil || !due {
		t.Fatalf("expected morning due at 09:00 the next day, got due=%v err=%v", due, err)
	}
}

// TestDue_OncePerDay: after running this morning, it must NOT fire again today,
// but it must fire tomorrow at the boundary.
func TestDue_OncePerDay(t *testing.T) {
	d := testDB(t)
	s := newSched(d)
	job := Job{Name: "morning", PeriodDays: 1, AtHour: 7}

	// Ran today at 07:05.
	ranToday := atDay(3, 7).Add(5 * time.Minute)
	if err := d.SetKV(kvPrefix+job.Name, ranToday.Format(time.RFC3339)); err != nil {
		t.Fatalf("setkv: %v", err)
	}
	// Later the same day: not due.
	if due, _ := s.due(job, atDay(3, 15)); due {
		t.Fatal("expected not due again the same day")
	}
	// Tomorrow past the boundary: due.
	if due, _ := s.due(job, atDay(4, 8)); !due {
		t.Fatal("expected due the next morning")
	}
}

// TestDue_WeeklyStaysWeekly: PeriodDays=7 keeps a weekly job from firing on
// consecutive daily boundaries.
func TestDue_WeeklyStaysWeekly(t *testing.T) {
	d := testDB(t)
	s := newSched(d)
	job := Job{Name: "weekly-health", PeriodDays: 7, AtHour: 9}

	ran := atDay(3, 9).Add(time.Minute)
	if err := d.SetKV(kvPrefix+job.Name, ran.Format(time.RFC3339)); err != nil {
		t.Fatalf("setkv: %v", err)
	}
	// Next day past 09:00: boundary passed, but Every (6d) has not - not due.
	if due, _ := s.due(job, atDay(4, 10)); due {
		t.Fatal("weekly job should not fire the next day")
	}
	// A week later: due.
	if due, _ := s.due(job, atDay(10, 10)); !due {
		t.Fatal("weekly job should fire ~a week later")
	}
}

func TestDue_RespectsEverySpacing(t *testing.T) {
	d := testDB(t)
	s := newSched(d)
	job := Job{Name: "nightly", Every: 20 * time.Hour, AtHour: -1}

	// Record a run "now".
	now := at(22)
	if err := d.SetKV(kvPrefix+job.Name, now.Format(time.RFC3339)); err != nil {
		t.Fatalf("setkv: %v", err)
	}
	// 1h later: too soon.
	if due, _ := s.due(job, now.Add(time.Hour)); due {
		t.Fatal("expected not due 1h after a run")
	}
	// 21h later: past Every.
	if due, _ := s.due(job, now.Add(21*time.Hour)); !due {
		t.Fatal("expected due 21h after a run")
	}
}

func TestFire_EnqueuesAndRecords(t *testing.T) {
	d := testDB(t)
	// Seed a default agent group + session so FK + open work.
	if _, _, err := d.Apply(db.Bootstrap{DefaultAgentGroupName: "default", DefaultAgentGroupFolder: "default"}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	dir := t.TempDir()
	target := Target{AgentGroupID: 1, SessionKey: "telegram:1", Channel: "telegram", ChatID: "1"}
	s := New(d, dir, nil, target, quiet())

	job := Job{Name: "nightly", Every: 20 * time.Hour, AtHour: -1, Prompt: "do maintenance"}
	now := at(22)
	if err := s.fire(context.Background(), job, now); err != nil {
		t.Fatalf("fire: %v", err)
	}

	// The prompt landed in the session inbound.
	sess, err := db.OpenSession(dir, 1, target.SessionKey)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() { _ = sess.Close() }()
	pend, _ := sess.PendingInbound()
	if len(pend) != 1 || pend[0].Text != "do maintenance" {
		t.Fatalf("expected the maintenance prompt enqueued, got %+v", pend)
	}

	// And last-run was recorded, so it's no longer due.
	if due, _ := s.due(job, now.Add(time.Minute)); due {
		t.Fatal("expected not due right after firing")
	}
}
