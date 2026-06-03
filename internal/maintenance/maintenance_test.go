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
	t.Cleanup(func() { d.Close() })
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
	job := Job{Name: "nightly", Every: 20 * time.Hour, AtHour: 22}

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
	defer sess.Close()
	pend, _ := sess.PendingInbound()
	if len(pend) != 1 || pend[0].Text != "do maintenance" {
		t.Fatalf("expected the maintenance prompt enqueued, got %+v", pend)
	}

	// And last-run was recorded, so it's no longer due.
	if due, _ := s.due(job, now.Add(time.Minute)); due {
		t.Fatal("expected not due right after firing")
	}
}
