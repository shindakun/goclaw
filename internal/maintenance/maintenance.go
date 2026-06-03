// Package maintenance runs the vault's scheduled upkeep (brief §11.5): the
// agent reconciles contradictions, synthesizes themes, lints, and builds the day
// note on a fixed cadence. A job is just a prompt enqueued into the vault
// session, run by the agent exactly like a user message; its short summary flows
// back to chat through the normal delivery path.
//
// Firing is a host ticker that checks each job against its last-run timestamp in
// the central DB's kv table, so schedules survive restarts and never double-fire.
package maintenance

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/shindakun/goclaw/internal/db"
	"github.com/shindakun/goclaw/internal/mounts"
)

// checkInterval is how often the scheduler evaluates due jobs. Jobs fire at most
// once per their period regardless of this cadence.
const checkInterval = 5 * time.Minute

// kvPrefix namespaces the last-run keys in the central kv table.
const kvPrefix = "maint:lastrun:"

// RunnerEnsurer ensures the agent-group runner is up so an enqueued maintenance
// prompt gets consumed. internal/runtime implements it; the variadic extra
// mounts match that signature (maintenance passes none).
type RunnerEnsurer interface {
	EnsureRunner(ctx context.Context, agentGroupID int64, groupDir string, extra ...mounts.Request) error
}

// Job is one scheduled maintenance task.
type Job struct {
	Name   string        // stable id, used in the kv last-run key
	Every  time.Duration // minimum spacing between runs
	AtHour int           // local hour-of-day to prefer (0-23); -1 means "any time once Every has elapsed"
	Prompt string        // the instruction enqueued for the agent
}

// DefaultJobs is the brief's §11.5 cadence. Times are local. Prompts lean on the
// vault CLAUDE.md operations (reconcile/synthesize/lint) and ask for a short
// chat summary so the owner stays informed without noise.
var DefaultJobs = []Job{
	{
		Name:   "morning",
		Every:  20 * time.Hour, // once a day, prefer the morning hour
		AtHour: 7,
		Prompt: "Maintenance (morning): create or update today's day note in wiki/daily/. " +
			"Pull any open/overdue tasks from wiki/tasks/ into it. Reply with a 1-2 sentence summary only.",
	},
	{
		Name:   "nightly",
		Every:  20 * time.Hour,
		AtHour: 22,
		Prompt: "Maintenance (nightly): run reconcile (resolve contradicting notes, not just flag them) " +
			"and synthesize (write synthesis pages for recurring un-named themes), then heal orphan pages. " +
			"Commit your vault changes with git. Reply with a 1-2 sentence summary only.",
	},
	{
		Name:   "weekly-health",
		Every:  6 * 24 * time.Hour, // weekly
		AtHour: 9,
		Prompt: "Maintenance (weekly health): run lint over the whole vault (broken links, duplicates, bad " +
			"frontmatter, stale claims, orphans, lingering unresolved_references). Report by severity; do NOT " +
			"auto-fix. Reply with a 1-2 sentence summary of the worst findings only.",
	},
}

// Target identifies where to enqueue maintenance prompts: a session and its
// agent group (so the runner can be ensured and the reply can be delivered).
type Target struct {
	AgentGroupID int64
	SessionKey   string
	Channel      string // origin channel, for the enqueued message
	ChatID       string // origin chat, so the summary is delivered there
}

// Scheduler fires maintenance jobs against a target session.
type Scheduler struct {
	central *db.DB
	dataDir string
	ensurer RunnerEnsurer
	target  Target
	jobs    []Job
	log     *slog.Logger
}

// New constructs a Scheduler. ensurer may be nil to skip ensuring the runner
// (the enqueue still happens). jobs defaults to DefaultJobs when empty.
func New(central *db.DB, dataDir string, ensurer RunnerEnsurer, target Target, log *slog.Logger) *Scheduler {
	return &Scheduler{
		central: central,
		dataDir: dataDir,
		ensurer: ensurer,
		target:  target,
		jobs:    DefaultJobs,
		log:     log,
	}
}

// Run evaluates due jobs every checkInterval until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	// Evaluate once shortly after start, then on the ticker.
	s.tick(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case t := <-ticker.C:
			s.tick(ctx, t)
		}
	}
}

// tick fires every job that is currently due.
func (s *Scheduler) tick(ctx context.Context, now time.Time) {
	for _, job := range s.jobs {
		due, err := s.due(job, now)
		if err != nil {
			s.log.Error("maintenance: due check", "job", job.Name, "err", err)
			continue
		}
		if !due {
			continue
		}
		if err := s.fire(ctx, job, now); err != nil {
			s.log.Error("maintenance: fire", "job", job.Name, "err", err)
			continue
		}
		s.log.Info("maintenance job fired", "job", job.Name)
	}
}

// due reports whether a job should run now: at least Every has elapsed since its
// last run, and (when AtHour >= 0) it is at or past that local hour today.
func (s *Scheduler) due(job Job, now time.Time) (bool, error) {
	last, ok, err := s.central.GetKV(kvPrefix + job.Name)
	if err != nil {
		return false, err
	}
	if ok {
		t, perr := time.Parse(time.RFC3339, last)
		if perr == nil && now.Sub(t) < job.Every {
			return false, nil // ran too recently
		}
	}
	if job.AtHour >= 0 && now.Hour() < job.AtHour {
		return false, nil // not yet at the preferred hour today
	}
	return true, nil
}

// fire enqueues the job's prompt into the target session and ensures the runner.
func (s *Scheduler) fire(ctx context.Context, job Job, now time.Time) error {
	if _, err := s.central.ResolveOrCreateSession(s.target.AgentGroupID, s.target.SessionKey); err != nil {
		return err
	}
	sess, err := db.OpenSession(s.dataDir, s.target.AgentGroupID, s.target.SessionKey)
	if err != nil {
		return err
	}
	defer sess.Close()

	if _, err := sess.EnqueueInbound(s.target.Channel, s.target.ChatID, "system", "maintenance", job.Prompt); err != nil {
		return err
	}

	if s.ensurer != nil {
		groupDir := db.AgentGroupDir(s.dataDir, s.target.AgentGroupID)
		if err := s.ensurer.EnsureRunner(ctx, s.target.AgentGroupID, groupDir); err != nil {
			// The prompt is queued; a later run or message will bring the runner up.
			s.log.Error("maintenance: ensure runner", "job", job.Name, "err", err)
		}
	}

	// Record the run so it doesn't re-fire until Every elapses again.
	if err := s.central.SetKV(kvPrefix+job.Name, now.Format(time.RFC3339)); err != nil {
		return fmt.Errorf("record last-run: %w", err)
	}
	return nil
}
