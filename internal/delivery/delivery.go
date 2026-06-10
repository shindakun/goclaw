// Package delivery polls session outbound.db files, enforces delivery
// authorization, and dispatches replies through the channel registry
// (brief §3.1, §7.3, §9).
//
// Delivery authorization (brief §9): an agent may deliver to its origin chat
// always, or to any channel/chat with an explicit agent_destinations row -
// checked HERE, before the adapter's Send is ever called.
package delivery

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/shindakun/goclaw/internal/channels"
	"github.com/shindakun/goclaw/internal/db"
	"github.com/shindakun/goclaw/internal/eventlog"
)

// pollInterval is how often the delivery loop drains outbound queues.
const pollInterval = 500 * time.Millisecond

// Typer stops a chat's "typing…" indicator once its reply has been delivered.
// internal/typing implements it; may be nil to disable.
type Typer interface {
	Stop(channel, chatID string)
}

// OutboundInterceptor lets the host act on an agent reply BEFORE it is delivered, e.g.
// run an agent-emitted "/schedule ..." directive against the DB instead of sending the
// raw command text to the user. Intercept returns (replacement, handled): when handled,
// the original text is NOT sent; replacement (if non-empty) is sent instead.
type OutboundInterceptor interface {
	Intercept(channel, chatID, text string) (replacement string, handled bool)
}

// Deliverer drains outbound messages and dispatches them.
type Deliverer struct {
	central     *db.DB
	registry    *channels.Registry
	dataDir     string
	typer       Typer
	interceptor OutboundInterceptor // optional; nil = no interception
	events      *eventlog.Logger    // optional; nil = no operational event log
	log         *slog.Logger
}

// New constructs a Deliverer. typer may be nil to disable typing-indicator
// teardown.
func New(central *db.DB, registry *channels.Registry, dataDir string, typer Typer, log *slog.Logger) *Deliverer {
	return &Deliverer{central: central, registry: registry, dataDir: dataDir, typer: typer, log: log}
}

// WithInterceptor sets an outbound interceptor (e.g. the router, to handle agent-emitted
// /schedule directives). Returns d for chaining.
func (d *Deliverer) WithInterceptor(i OutboundInterceptor) *Deliverer {
	d.interceptor = i
	return d
}

// WithEventLog sets the operational event log delivery records sent/denied/failed events
// into. Optional (nil-safe); returns d for chaining.
func (d *Deliverer) WithEventLog(e *eventlog.Logger) *Deliverer {
	d.events = e
	return d
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
	defer func() { _ = sess.Close() }()

	pending, err := sess.PendingOutbound()
	if err != nil {
		return err
	}
	originChannel, originChat := splitSessionKey(s.SessionKey)

	for _, m := range pending {
		// Dedup against the host-owned delivery ledger (inbound.db). The runner
		// owns outbound.db and may rewrite it (resurrecting a row we already
		// sent) any time, so 'pending' there is NOT a reliable "needs sending"
		// signal across the mount - the ledger is. If we've recorded this id,
		// skip it; this is what stops the same reply going out twice.
		done, err := sess.WasDelivered(m.ID)
		if err != nil {
			return err
		}
		if done {
			continue
		}

		ok, err := d.authorize(s.AgentGroupID, m.Channel, m.ChatID, originChannel, originChat)
		if err != nil {
			return err
		}
		if !ok {
			reason := "delivery not authorized for " + m.Channel + ":" + m.ChatID
			d.log.Warn("outbound denied", "session", s.SessionKey, "target", m.Channel+":"+m.ChatID)
			d.events.Emit(eventlog.KindDeliveryDenied, eventlog.Bool(false), map[string]any{
				"session": s.SessionKey, "channel": m.Channel, "chat": m.ChatID, "msg_id": m.ID,
			})
			if err := sess.MarkFailed(m.ID, reason); err != nil {
				return err
			}
			continue
		}

		// The reply is ready: the runner is done, so stop the typing indicator
		// regardless of whether the send below succeeds.
		if d.typer != nil {
			d.typer.Stop(m.Channel, m.ChatID)
		}

		// Intercept an agent-emitted directive (e.g. "/schedule ...") before sending:
		// the host runs it against the DB and sends its result, not the raw command.
		text := m.Text
		if d.interceptor != nil {
			if replacement, handled := d.interceptor.Intercept(m.Channel, m.ChatID, text); handled {
				if replacement == "" {
					// Nothing to send; just mark this outbound delivered so it is not retried.
					if err := sess.MarkDelivered(m.ID); err != nil {
						return err
					}
					continue
				}
				text = replacement
			}
		}

		out := channels.OutboundMsg{Channel: m.Channel, ChatID: m.ChatID, Text: text}
		if err := d.dispatch(ctx, out); err != nil {
			// Transient send failure: do NOT record it as terminally failed, so
			// the next drain retries. Dedup still protects us because the ledger
			// is only written on success below.
			d.log.Error("dispatch failed", "session", s.SessionKey, "msg_id", m.ID, "err", err)
			d.events.Emit(eventlog.KindDeliveryFailed, eventlog.Bool(false), map[string]any{
				"session": s.SessionKey, "channel": m.Channel, "chat": m.ChatID, "msg_id": m.ID,
			})
			continue
		}
		// Record delivery BEFORE logging success. The host owns inbound.db, so
		// this write is durable and can't be clobbered by the runner.
		if err := sess.MarkDelivered(m.ID); err != nil {
			return err
		}
		d.log.Info("delivered", "session", s.SessionKey, "target", m.Channel+":"+m.ChatID, "msg_id", m.ID)
		d.events.Emit(eventlog.KindDeliverySent, eventlog.Bool(true), map[string]any{
			"session": s.SessionKey, "channel": m.Channel, "chat": m.ChatID, "msg_id": m.ID,
		})
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
