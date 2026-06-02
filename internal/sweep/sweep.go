// Package sweep runs the periodic host sweep: runner recovery (relaunch a
// session's container if it died while work is still queued), and — later —
// stale-session detection, due-message wake, and recurrence (brief §3.3, §5).
package sweep

import (
	"context"
	"log/slog"
	"time"

	"github.com/shindakun/goclaw/internal/db"
)

// interval is the sweep cadence.
const interval = 60 * time.Second

// RunnerEnsurer makes sure a session's runner is up. internal/runtime
// implements it; it may be nil (no container orchestration).
type RunnerEnsurer interface {
	EnsureRunner(ctx context.Context, agentGroupID int64, sessionKey, sessionDir string) error
}

// Sweeper runs periodic maintenance over the central DB and sessions.
type Sweeper struct {
	central *db.DB
	dataDir string
	ensurer RunnerEnsurer
	log     *slog.Logger
}

// New constructs a Sweeper. ensurer may be nil to disable runner recovery.
func New(central *db.DB, dataDir string, ensurer RunnerEnsurer, log *slog.Logger) *Sweeper {
	return &Sweeper{central: central, dataDir: dataDir, ensurer: ensurer, log: log}
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
	// TODO: stale-session detection (mark/close idle sessions),
	// TODO: due-message wake (scheduled messages whose time has come),
	// TODO: recurrence (re-arm recurring scheduled tasks).
}

// recoverRunners ensures a runner is up for every active session that still has
// pending inbound work. This closes the silent-queue hole: if a session's
// container died (crash, manual removal, host restart) after its last message,
// nothing else would relaunch it, and messages would queue forever. EnsureRunner
// is idempotent, so sessions whose container is already running are untouched.
func (s *Sweeper) recoverRunners(ctx context.Context) {
	if s.ensurer == nil {
		return // no orchestration configured (runner started out of band)
	}
	sessions, err := s.central.ActiveSessions()
	if err != nil {
		s.log.Error("sweep: list sessions", "err", err)
		return
	}
	for _, sess := range sessions {
		pending, err := s.hasPendingInbound(sess)
		if err != nil {
			s.log.Error("sweep: check pending", "session", sess.SessionKey, "err", err)
			continue
		}
		if !pending {
			continue
		}
		dir := db.SessionDir(s.dataDir, sess.AgentGroupID, sess.SessionKey)
		if err := s.ensurer.EnsureRunner(ctx, sess.AgentGroupID, sess.SessionKey, dir); err != nil {
			s.log.Error("sweep: ensure runner", "session", sess.SessionKey, "err", err)
			continue
		}
		s.log.Info("sweep: ensured runner for queued session", "session", sess.SessionKey)
	}
}

// hasPendingInbound opens a session's DB pair and reports whether it has
// unconsumed inbound messages.
func (s *Sweeper) hasPendingInbound(sess db.Session) (bool, error) {
	sdb, err := db.OpenSession(s.dataDir, sess.AgentGroupID, sess.SessionKey)
	if err != nil {
		return false, err
	}
	defer sdb.Close()
	return sdb.HasPendingInbound()
}
