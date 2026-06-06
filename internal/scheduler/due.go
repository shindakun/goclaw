// Package scheduler runs user-definable recurring agent tasks
// (docs/scheduled-tasks.md). It generalizes the vault-maintenance scheduler: tasks live
// in the central DB (scheduled_tasks), each carries its own target and schedule, and the
// firing decision reuses the same restart-safe wall-clock algorithm maintenance proved
// (IsDue), keyed per task id in the kv last-run store.
package scheduler

import "time"

// IsDue reports whether a recurring task should fire at now, given when it last ran.
// The schedule is either a LOCAL wall-clock boundary (atHour 0-23, fires once per
// periodDays at/after that hour, in the host's local zone, so it does not drift with
// host uptime) OR a pure interval (atHour < 0: fire when now-lastRun >= every).
//
// ranBefore is false when the task has never run (no kv entry); lastRun is ignored then.
// This is the same algorithm internal/maintenance used; it lives here so both the
// maintenance jobs and user tasks share one correct implementation.
func IsDue(periodDays, atHour int, every time.Duration, lastRun time.Time, ranBefore bool, now time.Time) bool {
	// No preferred hour: pure interval scheduling.
	if atHour < 0 {
		return !ranBefore || now.Sub(lastRun) >= every
	}

	// Today's atHour boundary in the host's local zone.
	boundary := time.Date(now.Year(), now.Month(), now.Day(), atHour, 0, 0, 0, now.Location())
	if now.Before(boundary) {
		return false // not yet at the preferred hour today
	}
	// For a multi-day period, only fire if we are in a NEW period (enough days since the
	// last run), so the daily boundaries in between a weekly job do not trigger it.
	if ranBefore && periodDays > 1 && now.Sub(lastRun) < time.Duration(periodDays-1)*24*time.Hour {
		return false
	}
	// Due if we have not already run on/after today's boundary.
	return !ranBefore || lastRun.Before(boundary)
}
