// Package router resolves an inbound message's routing chain
// (user → messaging group → agent group → session), applies the access gate,
// and writes the message into the session's inbound.db (brief §3.2, §5).
//
// The router is pure host-side logic: it reads the central DB to resolve
// entities and the permissions package to gate access, then enqueues into
// inbound.db. It does not implement container orchestration itself — after a
// successful enqueue it asks an injected RunnerEnsurer to make sure a runner is
// up for that session, keeping the Podman dependency out of this package.
package router

import (
	"context"
	"log/slog"

	"github.com/shindakun/goclaw/internal/channels"
	"github.com/shindakun/goclaw/internal/db"
	"github.com/shindakun/goclaw/internal/permissions"
)

// RunnerEnsurer makes sure a runner is up for a session after a message is
// enqueued. internal/runtime implements this; it may be nil (no orchestration,
// e.g. when running the stub runner by hand or in tests).
type RunnerEnsurer interface {
	EnsureRunner(ctx context.Context, agentGroupID int64, sessionKey, sessionDir string) error
}

// Router routes inbound messages to session inbound DBs.
type Router struct {
	central *db.DB
	dataDir string
	ensurer RunnerEnsurer
	log     *slog.Logger

	// autoWireAgentGroupID, when non-zero, lets the owner bootstrap a wiring by
	// simply messaging the host: an owner message in an unwired conversation
	// auto-wires that conversation to this agent group (scope=owner). Off (0)
	// unless GOCLAW_AUTO_WIRE_OWNER is set. This is a convenience for first-run;
	// real deployments wire explicitly.
	autoWireAgentGroupID int64
}

// New constructs a Router over the central DB. autoWireAgentGroupID may be 0 to
// disable owner auto-wiring; ensurer may be nil to disable container launch.
func New(central *db.DB, dataDir string, autoWireAgentGroupID int64, ensurer RunnerEnsurer, log *slog.Logger) *Router {
	return &Router{
		central:              central,
		dataDir:              dataDir,
		ensurer:              ensurer,
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
		return r.enqueue(ctx, msg, wiring.AgentGroupID)
	}
	return nil
}

// enqueue resolves-or-creates the session for this conversation, opens its
// inbound.db, and writes the message as a pending row for the container to pick
// up (brief §3.1). v0 opens and closes the session DB pair per message; a future
// optimization is to cache open handles per active session.
func (r *Router) enqueue(ctx context.Context, msg channels.InboundMsg, agentGroupID int64) error {
	// One session per conversation in v0: the origin chat id is the session key.
	sessionKey := msg.Channel + ":" + msg.ChatID
	if _, err := r.central.ResolveOrCreateSession(agentGroupID, sessionKey); err != nil {
		return err
	}

	sess, err := db.OpenSession(r.dataDir, agentGroupID, sessionKey)
	if err != nil {
		return err
	}
	defer sess.Close()

	id, err := sess.EnqueueInbound(msg.Channel, msg.ChatID, msg.SenderID, msg.Sender, msg.Text)
	if err != nil {
		return err
	}
	r.log.Info("message enqueued to inbound",
		"channel", msg.Channel, "agent_group", agentGroupID, "session", sessionKey, "msg_id", id)

	// Make sure a runner is up to consume it. If no ensurer is configured, the
	// runner is expected to be started out of band (e.g. the stub runner by hand).
	if r.ensurer != nil {
		sessionDir := db.SessionDir(r.dataDir, agentGroupID, sessionKey)
		if err := r.ensurer.EnsureRunner(ctx, agentGroupID, sessionKey, sessionDir); err != nil {
			// Don't lose the message over a launch failure — it's safely queued
			// and a later message (or retry) can bring the runner up.
			r.log.Error("ensure runner failed (message remains queued)",
				"agent_group", agentGroupID, "session", sessionKey, "err", err)
		}
	}
	return nil
}
