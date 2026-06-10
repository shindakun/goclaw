package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/shindakun/goclaw/internal/db"
	"github.com/shindakun/goclaw/internal/eventlog"
	"github.com/shindakun/goclaw/internal/mounts"
)

// checkInterval is how often the scheduler evaluates due tasks. A task fires at most
// once per its own period regardless of this cadence. Matches the maintenance cadence.
const checkInterval = 5 * time.Minute

// kvPrefix namespaces a task's last-run key in the central kv table (by task id).
const kvPrefix = "task:lastrun:"

// RunnerEnsurer ensures the agent-group runner is up so an enqueued task prompt gets
// consumed. internal/runtime implements it.
type RunnerEnsurer interface {
	EnsureRunner(ctx context.Context, agentGroupID int64, groupDir string, extra ...mounts.Request) error
}

// Scheduler fires user-definable scheduled tasks loaded from the central DB. Unlike the
// maintenance scheduler (fixed jobs, one target), it loads tasks each tick and fires each
// into its OWN target (session/channel/chat). ensurer may be nil to skip ensuring the
// runner (the enqueue still happens).
type Scheduler struct {
	central *db.DB
	dataDir string
	ensurer RunnerEnsurer
	events  *eventlog.Logger // optional; nil = no operational event log
	log     *slog.Logger
	now     func() time.Time // injectable for tests
}

// New constructs a Scheduler.
func New(central *db.DB, dataDir string, ensurer RunnerEnsurer, log *slog.Logger) *Scheduler {
	return &Scheduler{central: central, dataDir: dataDir, ensurer: ensurer, log: log, now: time.Now}
}

// WithEventLog sets the operational event log fired/deferred events are recorded into.
// Optional (nil-safe); returns s for chaining.
func (s *Scheduler) WithEventLog(e *eventlog.Logger) *Scheduler {
	s.events = e
	return s
}

// Run evaluates due tasks every checkInterval until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	s.Tick(ctx, s.now()) // evaluate once shortly after start
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			s.Tick(ctx, s.now())
		}
	}
}

// Tick fires every task that is currently due. Exported so tests can drive it with a
// pinned clock.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) {
	tasks, err := s.central.EnabledScheduledTasks()
	if err != nil {
		s.log.Error("scheduler: load tasks", "err", err)
		return
	}
	for _, t := range tasks {
		due, err := s.due(t, now)
		if err != nil {
			s.log.Error("scheduler: due check", "task", t.Name, "err", err)
			continue
		}
		if !due {
			continue
		}
		if err := s.fire(ctx, t, now); err != nil {
			s.log.Error("scheduler: fire", "task", t.Name, "err", err)
			// A due task that could not be handed off (e.g. runner ensure failed in an
			// outage). It was NOT enqueued or stamped, so it re-fires next tick; record
			// that it deferred rather than completing.
			s.events.Emit(eventlog.KindScheduleDefer, eventlog.Bool(false), map[string]any{
				"task": t.Name, "owner": t.OwnerUserID, "reason": err.Error(),
			})
			continue
		}
		s.log.Info("scheduled task fired", "task", t.Name, "owner", t.OwnerUserID)
		s.events.Emit(eventlog.KindScheduleFired, eventlog.Bool(true), map[string]any{
			"task": t.Name, "owner": t.OwnerUserID,
		})
	}
}

// due loads the task's last-run from kv (keyed by id) and asks IsDue.
func (s *Scheduler) due(t db.ScheduledTask, now time.Time) (bool, error) {
	last, ok, err := s.central.GetKV(kvPrefix + t.ID)
	if err != nil {
		return false, err
	}
	var lastRun time.Time
	if ok {
		if parsed, perr := time.Parse(time.RFC3339, last); perr == nil {
			lastRun = parsed
		}
	}
	return IsDue(t.PeriodDays, t.AtHour, t.AtMinute, time.Duration(t.EverySeconds)*time.Second, lastRun, ok, now), nil
}

// fire hands the task's prompt to its target session and records the run. The order is
// deliberate so a task is marked fired ONLY once the work is durably handed off:
//
//  1. ensure the runner is up FIRST. If it cannot be started (e.g. a network outage at
//     07:00), abort WITHOUT enqueuing or stamping last-run, so the task re-fires on the next
//     tick instead of being marked done while nothing ran. (This is the "morning routine
//     fired during a network outage and was lost" bug: firing must not count as completing.)
//  2. only then enqueue the prompt into inbound.db (durable), and
//  3. stamp last-run.
//
// Note the runner being UP is the bar for "handed off", not the agent COMPLETING the turn:
// the scheduler cannot see across the boundary whether the agent finished (that is the
// runner's job, which now leaves a failed turn's message queued for retry rather than
// consuming it). Together: the scheduler won't mark a task done if it could not even hand
// the work off, and the runner won't drop the work if the agent turn then fails transiently.
func (s *Scheduler) fire(ctx context.Context, t db.ScheduledTask, now time.Time) error {
	if _, err := s.central.ResolveOrCreateSession(t.AgentGroupID, t.SessionKey); err != nil {
		return err
	}

	// Ensure the runner BEFORE enqueuing/stamping. A failure here means we could not hand
	// the work off at all, so do not enqueue (avoids a stranded prompt) and do not stamp
	// (so it re-fires next tick).
	if s.ensurer != nil {
		groupDir := db.AgentGroupDir(s.dataDir, t.AgentGroupID)
		if err := s.ensurer.EnsureRunner(ctx, t.AgentGroupID, groupDir); err != nil {
			return fmt.Errorf("scheduler: ensure runner (will re-fire next tick): %w", err)
		}
	}

	sess, err := db.OpenSession(s.dataDir, t.AgentGroupID, t.SessionKey)
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()

	if _, err := sess.EnqueueInbound(t.Channel, t.ChatID, "system", "scheduler", t.Prompt); err != nil {
		return err
	}

	if err := s.central.SetKV(kvPrefix+t.ID, now.Format(time.RFC3339)); err != nil {
		return fmt.Errorf("record last-run: %w", err)
	}
	return nil
}
