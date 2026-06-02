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

	// autoWireAgentGroupID, when non-zero, lets the owner bootstrap a wiring by
	// simply messaging the host: an owner message in an unwired conversation
	// auto-wires that conversation to this agent group (scope=owner). Off (0)
	// unless GOCLAW_AUTO_WIRE_OWNER is set. This is a convenience for first-run;
	// real deployments wire explicitly.
	autoWireAgentGroupID int64
}

// New constructs a Router over the central DB. autoWireAgentGroupID may be 0 to
// disable owner auto-wiring.
func New(central *db.DB, dataDir string, autoWireAgentGroupID int64, log *slog.Logger) *Router {
	return &Router{
		central:              central,
		dataDir:              dataDir,
		log:                  log,
		autoWireAgentGroupID: autoWireAgentGroupID,
	}
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
	// Resolve the routing chain from the central DB (brief §3.2).
	user, err := r.central.UserByIdentity(msg.Channel, msg.SenderID)
	if err != nil {
		return err
	}

	// Record the conversation on first contact so it becomes routable, then
	// resolve its wiring.
	mgID, err := r.central.UpsertMessagingGroup(msg.Channel, msg.ChatID, "")
	if err != nil {
		return err
	}
	wiring, err := r.central.WiringForMessagingGroup(mgID)
	if err != nil {
		return err
	}
	if wiring == nil {
		// First-run convenience: if the owner messages an unwired conversation
		// and auto-wire is enabled, wire it to the default agent group so the
		// path works without manual DB setup. Otherwise, drop.
		if r.autoWireAgentGroupID != 0 && user != nil && user.Role == string(permissions.RoleOwner) {
			if _, err := r.central.EnsureWiring(mgID, r.autoWireAgentGroupID,
				string(permissions.ScopeOwner), string(permissions.PolicyStrict)); err != nil {
				return err
			}
			r.log.Info("owner auto-wired conversation",
				"channel", msg.Channel, "chat", msg.ChatID, "agent_group", r.autoWireAgentGroupID)
			if wiring, err = r.central.WiringForMessagingGroup(mgID); err != nil {
				return err
			}
		} else {
			r.log.Info("message dropped: no wiring for conversation",
				"channel", msg.Channel, "chat", msg.ChatID)
			return nil
		}
	}

	// Build the access request from resolved facts (brief §9).
	req := permissions.Request{
		KnownUser: user != nil,
		Scope:     permissions.SenderScope(wiring.SenderScope),
		Policy:    permissions.UnknownSenderPolicy(wiring.UnknownSenderPolicy),
	}
	if user != nil {
		req.Role = permissions.Role(user.Role)
	}

	switch permissions.Check(req) {
	case permissions.Deny:
		r.log.Info("message denied", "channel", msg.Channel, "sender", msg.SenderID, "known", user != nil)
		return nil
	case permissions.NeedsApproval:
		r.log.Info("message needs approval", "channel", msg.Channel, "sender", msg.SenderID)
		// TODO: emit approval card.
		return nil
	case permissions.Allow:
		// TODO: resolve-or-create the session, write msg into its inbound.db,
		// and wake the container.
		r.log.Info("message routed (stub)",
			"channel", msg.Channel, "agent_group", wiring.AgentGroupID, "text", msg.Text)
		return nil
	}
	return nil
}
