// Package delivery polls session outbound.db files, enforces delivery
// authorization, and dispatches replies through the channel registry
// (brief §3.1, §7.3, §9).
//
// Delivery authorization (brief §9): an agent may deliver to its origin chat
// always, or to any channel/chat with an explicit agent_destinations row —
// checked HERE, before the adapter's Send is ever called.
package delivery

import (
	"context"
	"log/slog"
	"time"

	"github.com/shindakun/goclaw/internal/channels"
	"github.com/shindakun/goclaw/internal/db"
)

// pollInterval is how often the delivery loop drains outbound queues.
const pollInterval = 500 * time.Millisecond

// Deliverer drains outbound messages and dispatches them.
type Deliverer struct {
	central  *db.DB
	registry *channels.Registry
	log      *slog.Logger
}

// New constructs a Deliverer.
func New(central *db.DB, registry *channels.Registry, log *slog.Logger) *Deliverer {
	return &Deliverer{central: central, registry: registry, log: log}
}

// Run polls outbound queues on a ticker until ctx is cancelled.
func (d *Deliverer) Run(ctx context.Context) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := d.drain(ctx); err != nil {
				d.log.Error("drain outbound", "err", err)
			}
		}
	}
}

// drain reads pending outbound rows across active sessions and dispatches them.
func (d *Deliverer) drain(ctx context.Context) error {
	// TODO: enumerate active sessions, open each outbound.db (host is a reader),
	// SELECT undelivered rows, and for each:
	//   1. authorize(channel, chatID, agentGroupID) — origin chat or
	//      agent_destinations row,
	//   2. look up the adapter in the registry,
	//   3. adapter.Send(ctx, out),
	//   4. mark the row delivered.
	return nil
}

// dispatch sends one authorized message via its channel adapter.
func (d *Deliverer) dispatch(ctx context.Context, out channels.OutboundMsg) error {
	adapter, ok := d.registry.Get(out.Channel)
	if !ok {
		d.log.Warn("no adapter for channel", "channel", out.Channel)
		return nil
	}
	return adapter.Send(ctx, out)
}

// authorize enforces origin-chat-always-allowed + agent_destinations (brief §9).
//
// TODO: implement against the central DB. Default-deny.
func (d *Deliverer) authorize(agentGroupID int64, channel, chatID string) bool {
	return false
}
