// Package sweep runs the 60-second host sweep: stale-session detection,
// due-message wake, and recurrence (brief §3.3, §5).
package sweep

import (
	"context"
	"log/slog"
	"time"

	"github.com/shindakun/goclaw/internal/db"
)

// interval is the sweep cadence.
const interval = 60 * time.Second

// Sweeper runs periodic maintenance over the central DB and sessions.
type Sweeper struct {
	central *db.DB
	log     *slog.Logger
}

// New constructs a Sweeper.
func New(central *db.DB, log *slog.Logger) *Sweeper {
	return &Sweeper{central: central, log: log}
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
	// TODO: stale-session detection (mark/close idle sessions),
	// TODO: due-message wake (scheduled messages whose time has come),
	// TODO: recurrence (re-arm recurring scheduled tasks).
	s.log.Debug("sweep tick (stub)")
}
