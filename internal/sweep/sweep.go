// Package sweep runs the periodic host sweep: runner recovery (relaunch a
// session's container if it died while work is still queued), and - later -
// stale-session detection, due-message wake, and recurrence (brief §3.3, §5).
package sweep

import (
	"context"
	"log/slog"
	"time"

	"github.com/shindakun/goclaw/internal/db"
	"github.com/shindakun/goclaw/internal/mounts"
)

// interval is the sweep cadence.
const interval = 60 * time.Second

// idleTTL is how long an agent group can go without session activity before its
// runner container is reaped. Must exceed the sweep interval so a just-active
// group isn't reaped between ticks.
const idleTTL = 10 * time.Minute

// RunnerManager makes sure an agent group's runner is up and can reap idle
// runners. internal/runtime implements it; it may be nil (no orchestration).
type RunnerManager interface {
	EnsureRunner(ctx context.Context, agentGroupID int64, groupDir string, extra ...mounts.Request) error
	RunningGroupIDs(ctx context.Context) ([]int64, error)
	StopGroupRunner(ctx context.Context, agentGroupID int64) error
}

// Sweeper runs periodic maintenance over the central DB and sessions.
type Sweeper struct {
	central *db.DB
	dataDir string
	runners RunnerManager
	log     *slog.Logger
}

// New constructs a Sweeper. runners may be nil to disable runner recovery + GC.
func New(central *db.DB, dataDir string, runners RunnerManager, log *slog.Logger) *Sweeper {
	return &Sweeper{central: central, dataDir: dataDir, runners: runners, log: log}
}

// Run ticks every interval until ctx is cancelled.
func (s *Sweeper) Run(ctx context.Context) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick performs one sweep pass.
func (s *Sweeper) tick(ctx context.Context) {
	s.recoverRunners(ctx)
	s.gcIdleRunners(ctx, time.Now())
	// TODO: due-message wake (scheduled messages whose time has come),
	// TODO: recurrence (re-arm recurring scheduled tasks).
}

// recoverRunners ensures a runner is up for every active session that still has
// pending inbound work. This closes the silent-queue hole: if a session's
// container died (crash, manual removal, host restart) after its last message,
// nothing else would relaunch it, and messages would queue forever. EnsureRunner
// is idempotent, so sessions whose container is already running are untouched.
func (s *Sweeper) recoverRunners(ctx context.Context) {
	if s.runners == nil {
		return // no orchestration configured (runner started out of band)
	}
	sessions, err := s.central.ActiveSessions()
	if err != nil {
		s.log.Error("sweep: list sessions", "err", err)
		return
	}
	// One container per agent group, so collect the set of groups that have any
	// session with pending inbound, then ensure each group's runner once.
	needsRunner := make(map[int64]bool)
	for _, sess := range sessions {
		if needsRunner[sess.AgentGroupID] {
			continue // already know this group needs its runner
		}
		pending, err := s.hasPendingInbound(sess)
		if err != nil {
			s.log.Error("sweep: check pending", "session", sess.SessionKey, "err", err)
			continue
		}
		if pending {
			needsRunner[sess.AgentGroupID] = true
		}
	}
	for agentGroupID := range needsRunner {
		dir := db.AgentGroupDir(s.dataDir, agentGroupID)
		extra, err := s.extraMountRequests(agentGroupID)
		if err != nil {
			s.log.Error("sweep: load agent mounts", "agent_group", agentGroupID, "err", err)
			continue
		}
		if err := s.runners.EnsureRunner(ctx, agentGroupID, dir, extra...); err != nil {
			s.log.Error("sweep: ensure runner", "agent_group", agentGroupID, "err", err)
			continue
		}
		s.log.Info("sweep: ensured runner for queued group", "agent_group", agentGroupID)
	}
}

// extraMountRequests loads an agent group's extra mounts for launch (so a
// recovered runner gets the same mounts as a router-launched one).
func (s *Sweeper) extraMountRequests(agentGroupID int64) ([]mounts.Request, error) {
	ams, err := s.central.AgentMounts(agentGroupID)
	if err != nil {
		return nil, err
	}
	reqs := make([]mounts.Request, 0, len(ams))
	for _, am := range ams {
		reqs = append(reqs, mounts.Request{
			HostPath:      am.HostPath,
			ContainerPath: am.ContainerPath,
			ReadWrite:     am.ReadWrite,
		})
	}
	return reqs, nil
}

// gcIdleRunners stops runner containers for agent groups that have had no
// session activity within idleTTL - reaping idle runners so they don't
// accumulate. A group is kept alive if it was recently active OR still has
// pending inbound (which recoverRunners just (re)launched). now is passed in so
// the cutoff is testable.
func (s *Sweeper) gcIdleRunners(ctx context.Context, now time.Time) {
	if s.runners == nil {
		return
	}
	running, err := s.runners.RunningGroupIDs(ctx)
	if err != nil {
		s.log.Error("sweep: list running runners", "err", err)
		return
	}
	if len(running) == 0 {
		return
	}

	cutoff := now.Add(-idleTTL).UTC().Format("2006-01-02 15:04:05")
	active, err := s.central.AgentGroupIDsActiveSince(cutoff)
	if err != nil {
		s.log.Error("sweep: active groups", "err", err)
		return
	}

	for _, agentGroupID := range running {
		if active[agentGroupID] {
			continue // recently active - keep its runner
		}
		// Don't reap a group that still has queued work waiting to be consumed.
		pending, err := s.groupHasPendingInbound(agentGroupID)
		if err != nil {
			s.log.Error("sweep: gc check pending", "agent_group", agentGroupID, "err", err)
			continue
		}
		if pending {
			continue
		}
		if err := s.runners.StopGroupRunner(ctx, agentGroupID); err != nil {
			s.log.Error("sweep: stop idle runner", "agent_group", agentGroupID, "err", err)
			continue
		}
		s.log.Info("sweep: reaped idle runner", "agent_group", agentGroupID)
	}
}

// groupHasPendingInbound reports whether any session in the group has pending
// inbound messages.
func (s *Sweeper) groupHasPendingInbound(agentGroupID int64) (bool, error) {
	sessions, err := s.central.ActiveSessions()
	if err != nil {
		return false, err
	}
	for _, sess := range sessions {
		if sess.AgentGroupID != agentGroupID {
			continue
		}
		pending, err := s.hasPendingInbound(sess)
		if err != nil {
			return false, err
		}
		if pending {
			return true, nil
		}
	}
	return false, nil
}

// hasPendingInbound opens a session's DB pair and reports whether it has
// unconsumed inbound messages.
func (s *Sweeper) hasPendingInbound(sess db.Session) (bool, error) {
	sdb, err := db.OpenSession(s.dataDir, sess.AgentGroupID, sess.SessionKey)
	if err != nil {
		return false, err
	}
	defer func() { _ = sdb.Close() }()
	return sdb.HasPendingInbound()
}
