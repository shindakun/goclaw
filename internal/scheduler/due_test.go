package scheduler

import (
	"testing"
	"time"
)

func TestIsDue_DailyAtHour(t *testing.T) {
	loc := time.Local
	// A daily task at 08:00.
	at := 8
	day := func(h, m int) time.Time { return time.Date(2026, 6, 6, h, m, 0, 0, loc) }

	// Never run, before 08:00 today -> not due.
	if IsDue(1, at, 0, time.Time{}, false, day(7, 59)) {
		t.Error("before the boundary, never-run task should not be due")
	}
	// Never run, at/after 08:00 -> due.
	if !IsDue(1, at, 0, time.Time{}, false, day(8, 1)) {
		t.Error("at the boundary, never-run task should be due")
	}
	// Ran yesterday, now after 08:00 today -> due (a new daily boundary).
	lastRun := time.Date(2026, 6, 5, 8, 0, 0, 0, loc)
	if !IsDue(1, at, 0, lastRun, true, day(8, 5)) {
		t.Error("ran yesterday, after today's boundary -> due")
	}
	// Already ran today (after the boundary) -> not due again today.
	ranToday := day(8, 1)
	if IsDue(1, at, 0, ranToday, true, day(20, 0)) {
		t.Error("already ran today -> not due again today")
	}
}

func TestIsDue_WeeklyAtHour(t *testing.T) {
	loc := time.Local
	at := 9
	// Ran 2 days ago: a weekly (period 7) task must NOT fire on the intervening daily
	// boundaries.
	lastRun := time.Date(2026, 6, 4, 9, 0, 0, 0, loc)
	now := time.Date(2026, 6, 6, 9, 30, 0, 0, loc) // 2 days later, past 09:00
	if IsDue(7, at, 0, lastRun, true, now) {
		t.Error("weekly task should not fire only 2 days after last run")
	}
	// 8 days later, past 09:00 -> due.
	now8 := time.Date(2026, 6, 12, 9, 30, 0, 0, loc)
	if !IsDue(7, at, 0, lastRun, true, now8) {
		t.Error("weekly task should fire 8 days after last run")
	}
}

func TestIsDue_PureInterval(t *testing.T) {
	// atHour < 0 -> pure interval scheduling on Every.
	every := 30 * time.Minute
	base := time.Now()
	// Never run -> due immediately.
	if !IsDue(1, -1, every, time.Time{}, false, base) {
		t.Error("never-run interval task should be due immediately")
	}
	// Ran 10m ago, interval 30m -> not due.
	if IsDue(1, -1, every, base.Add(-10*time.Minute), true, base) {
		t.Error("interval task not due before the interval elapses")
	}
	// Ran 31m ago -> due.
	if !IsDue(1, -1, every, base.Add(-31*time.Minute), true, base) {
		t.Error("interval task due after the interval elapses")
	}
}
