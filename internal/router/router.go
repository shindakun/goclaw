// Package router resolves an inbound message's routing chain
// (user → messaging group → agent group → session), applies the access gate,
// and writes the message into the session's inbound.db (brief §3.2, §5).
//
// The router is pure host-side logic: it reads the central DB to resolve
// entities and the permissions package to gate access, then enqueues into
// inbound.db. It never talks to a channel or a container directly.
package router

import (
	"context"
	"log/slog"

	"github.com/shindakun/goclaw/internal/channels"
	"github.com/shindakun/goclaw/internal/db"
	"github.com/shindakun/goclaw/internal/permissions"
)

// Router routes inbound messages to session inbound DBs.
type Router struct {
	central *db.DB
	dataDir string
	log     *slog.Logger
}

// New constructs a Router over the central DB.
func New(central *db.DB, dataDir string, log *slog.Logger) *Router {
	return &Router{central: central, dataDir: dataDir, log: log}
}

// Run drains inbound messages until ctx is cancelled, routing each one.
func (r *Router) Run(ctx context.Context, in <-chan channels.InboundMsg) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-in:
			if !ok {
				return nil
			}
			if err := r.route(ctx, msg); err != nil {
				r.log.Error("route message", "channel", msg.Channel, "chat", msg.ChatID, "err", err)
			}
		}
	}
}

// route resolves the chain for one message, gates it, and enqueues it.
func (r *Router) route(ctx context.Context, msg channels.InboundMsg) error {
	// TODO: resolve user from (channel, sender_id) via user_identities.
	// TODO: resolve messaging group from (channel, chat_id).
	// TODO: resolve the wiring → agent group + sender_scope + policy.
	// TODO: resolve-or-create the session and open its inbound.db.

	// Access gate (brief §9). Placeholder request until resolution is wired.
	decision := permissions.Check(permissions.Request{
		KnownUser: false,
		Scope:     permissions.ScopeAll,
		Policy:    permissions.PolicyStrict,
	})
	switch decision {
	case permissions.Deny:
		r.log.Info("message denied", "channel", msg.Channel, "sender", msg.SenderID)
		return nil
	case permissions.NeedsApproval:
		r.log.Info("message needs approval", "channel", msg.Channel, "sender", msg.SenderID)
		// TODO: emit approval card.
		return nil
	case permissions.Allow:
		// TODO: write msg into the resolved session's inbound.db and wake the
		// container.
		r.log.Info("message routed (stub)", "channel", msg.Channel, "text", msg.Text)
		return nil
	}
	return nil
}
