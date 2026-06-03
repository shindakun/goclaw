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
	"strings"
	"time"

	"github.com/shindakun/goclaw/internal/channels"
	"github.com/shindakun/goclaw/internal/db"
)

// pollInterval is how often the delivery loop drains outbound queues.
const pollInterval = 500 * time.Millisecond

// Typer stops a chat's "typing…" indicator once its reply has been delivered.
// internal/typing implements it; may be nil to disable.
type Typer interface {
	Stop(channel, chatID string)
}

// Deliverer drains outbound messages and dispatches them.
type Deliverer struct {
	central  *db.DB
	registry *channels.Registry
	dataDir  string
	typer    Typer
	log      *slog.Logger
}

// New constructs a Deliverer. typer may be nil to disable typing-indicator
// teardown.
func New(central *db.DB, registry *channels.Registry, dataDir string, typer Typer, log *slog.Logger) *Deliverer {
	return &Deliverer{central: central, registry: registry, dataDir: dataDir, typer: typer, log: log}
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
	sessions, err := d.central.ActiveSessions()
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if err := d.drainSession(ctx, s); err != nil {
			d.log.Error("drain session", "session", s.SessionKey, "err", err)
			// Keep going with other sessions.
		}
	}
	return nil
}

// drainSession dispatches the pending outbound rows for one session.
func (d *Deliverer) drainSession(ctx context.Context, s db.Session) error {
	sess, err := db.OpenSession(d.dataDir, s.AgentGroupID, s.SessionKey)
	if err != nil {
		return err
	}
	defer sess.Close()

	pending, err := sess.PendingOutbound()
	if err != nil {
		return err
	}
	originChannel, originChat := splitSessionKey(s.SessionKey)

	for _, m := range pending {
		ok, err := d.authorize(s.AgentGroupID, m.Channel, m.ChatID, originChannel, originChat)
		if err != nil {
			return err
		}
		if !ok {
			reason := "delivery not authorized for " + m.Channel + ":" + m.ChatID
			d.log.Warn("outbound denied", "session", s.SessionKey, "target", m.Channel+":"+m.ChatID)
			if err := sess.MarkOutboundFailed(m.ID, reason); err != nil {
				return err
			}
			continue
		}

		// The reply is ready: the runner is done, so stop the typing indicator
		// regardless of whether the send below succeeds.
		if d.typer != nil {
			d.typer.Stop(m.Channel, m.ChatID)
		}

		out := channels.OutboundMsg{Channel: m.Channel, ChatID: m.ChatID, Text: m.Text}
		if err := d.dispatch(ctx, out); err != nil {
			d.log.Error("dispatch failed", "session", s.SessionKey, "err", err)
			if mErr := sess.MarkOutboundFailed(m.ID, err.Error()); mErr != nil {
				return mErr
			}
			continue
		}
		if err := sess.MarkOutboundDelivered(m.ID); err != nil {
			return err
		}
		d.log.Info("delivered", "session", s.SessionKey, "target", m.Channel+":"+m.ChatID, "msg_id", m.ID)
	}
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
// The origin chat (the conversation this session belongs to) is always allowed;
// any other target needs an explicit agent_destinations row.
func (d *Deliverer) authorize(agentGroupID int64, channel, chatID, originChannel, originChat string) (bool, error) {
	if channel == originChannel && chatID == originChat {
		return true, nil
	}
	return d.central.HasAgentDestination(agentGroupID, channel, chatID)
}

// splitSessionKey parses a "channel:chatID" session key into its parts. v0 keys
// are produced by the router as channel + ":" + chatID.
func splitSessionKey(key string) (channel, chatID string) {
	if c, rest, ok := strings.Cut(key, ":"); ok {
		return c, rest
	}
	return "", key
}
