package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/shindakun/goclaw/internal/db"
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
	log     *slog.Logger
	now     func() time.Time // injectable for tests
}

// New constructs a Scheduler.
func New(central *db.DB, dataDir string, ensurer RunnerEnsurer, log *slog.Logger) *Scheduler {
	return &Scheduler{central: central, dataDir: dataDir, ensurer: ensurer, log: log, now: time.Now}
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
			continue
		}
		s.log.Info("scheduled task fired", "task", t.Name, "owner", t.OwnerUserID)
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

// fire enqueues the task's prompt into its target session, ensures the runner, and
// records the run (BEFORE the agent processes it, so a transient agent failure does not
// re-fire: the prompt is durably queued and consumed when the agent recovers).
func (s *Scheduler) fire(ctx context.Context, t db.ScheduledTask, now time.Time) error {
	if _, err := s.central.ResolveOrCreateSession(t.AgentGroupID, t.SessionKey); err != nil {
		return err
	}
	sess, err := db.OpenSession(s.dataDir, t.AgentGroupID, t.SessionKey)
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()

	if _, err := sess.EnqueueInbound(t.Channel, t.ChatID, "system", "scheduler", t.Prompt); err != nil {
		return err
	}

	if s.ensurer != nil {
		groupDir := db.AgentGroupDir(s.dataDir, t.AgentGroupID)
		if err := s.ensurer.EnsureRunner(ctx, t.AgentGroupID, groupDir); err != nil {
			// The prompt is queued; a later tick or message brings the runner up.
			s.log.Error("scheduler: ensure runner", "task", t.Name, "err", err)
		}
	}

	if err := s.central.SetKV(kvPrefix+t.ID, now.Format(time.RFC3339)); err != nil {
		return fmt.Errorf("record last-run: %w", err)
	}
	return nil
}
