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
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/shindakun/goclaw/internal/channels"
	"github.com/shindakun/goclaw/internal/db"
	"github.com/shindakun/goclaw/internal/mounts"
	"github.com/shindakun/goclaw/internal/permissions"
)

// RunnerEnsurer makes sure a runner is up for an agent group after a message is
// enqueued. One container per agent group serves all its sessions, plus any
// extra mounts the group requested (validated against the allowlist by the
// implementation). internal/runtime implements this; it may be nil (no
// orchestration, e.g. when running the stub runner by hand or in tests).
type RunnerEnsurer interface {
	EnsureRunner(ctx context.Context, agentGroupID int64, groupDir string, extra ...mounts.Request) error
}

// Sender sends a host-originated reply on a channel — used for the approval-card
// flow (notifying the owner, confirming approve/deny). internal/channels.Registry
// satisfies this. May be nil to disable host-sent messages.
type Sender interface {
	Send(ctx context.Context, out channels.OutboundMsg) error
}

// Router routes inbound messages to session inbound DBs.
type Router struct {
	central *db.DB
	dataDir string
	ensurer RunnerEnsurer
	sender  Sender
	log     *slog.Logger

	// autoWireAgentGroupID, when non-zero, lets the owner bootstrap a wiring by
	// simply messaging the host: an owner message in an unwired conversation
	// auto-wires that conversation to this agent group (scope=owner). Off (0)
	// unless GOCLAW_AUTO_WIRE_OWNER is set. This is a convenience for first-run;
	// real deployments wire explicitly.
	autoWireAgentGroupID int64
}

// New constructs a Router over the central DB. autoWireAgentGroupID may be 0 to
// disable owner auto-wiring; ensurer may be nil to disable container launch;
// sender may be nil to disable host-sent messages (the approval-card flow).
func New(central *db.DB, dataDir string, autoWireAgentGroupID int64, ensurer RunnerEnsurer, sender Sender, log *slog.Logger) *Router {
	return &Router{
		central:              central,
		dataDir:              dataDir,
		ensurer:              ensurer,
		sender:               sender,
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

	// Owner/admin approval commands are handled before normal routing.
	if handled, err := r.handleApprovalCommand(ctx, msg, user); err != nil {
		return err
	} else if handled {
		return nil
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
		return r.requestApproval(ctx, msg, wiring.AgentGroupID)
	case permissions.Allow:
		return r.enqueue(ctx, msg, wiring.AgentGroupID)
	}
	return nil
}

// requestApproval holds an unknown sender's message and notifies the owner with
// an approval card (brief §3.4). A repeat message updates the held text rather
// than creating a duplicate request.
func (r *Router) requestApproval(ctx context.Context, msg channels.InboundMsg, agentGroupID int64) error {
	id, err := r.central.UpsertPendingApproval(db.PendingApproval{
		Channel:      msg.Channel,
		ChatID:       msg.ChatID,
		SenderID:     msg.SenderID,
		SenderName:   msg.Sender,
		Text:         msg.Text,
		AgentGroupID: agentGroupID,
	})
	if err != nil {
		return err
	}
	r.log.Info("message needs approval",
		"channel", msg.Channel, "sender", msg.SenderID, "approval_id", id)

	// Notify the owner of this agent group with the approval card.
	owner, err := r.central.OwnerIdentity(agentGroupID)
	if err != nil {
		return err
	}
	if owner == nil || r.sender == nil {
		// No reachable owner (or no sender wired) — the request is still held in
		// the DB and can be approved out of band.
		return nil
	}
	card := fmt.Sprintf(
		"Access request for agent group %d\nFrom: %s (%s on %s)\nMessage: %q\n\nReply /approve %d or /deny %d",
		agentGroupID, displayName(msg), msg.SenderID, msg.Channel, msg.Text, id, id)
	return r.sender.Send(ctx, channels.OutboundMsg{
		Channel: owner.Channel,
		ChatID:  owner.SenderID,
		Text:    card,
	})
}

// handleApprovalCommand intercepts "/approve <id>" and "/deny <id>" from an
// owner or admin. Returns (handled, err): when handled, the message is consumed
// and normal routing is skipped.
func (r *Router) handleApprovalCommand(ctx context.Context, msg channels.InboundMsg, user *db.User) (bool, error) {
	cmd, idStr, ok := parseApprovalCommand(msg.Text)
	if !ok {
		return false, nil
	}
	// Only owners/admins may approve.
	if user == nil || (user.Role != string(permissions.RoleOwner) && user.Role != string(permissions.RoleAdmin)) {
		return false, nil // not authorized — fall through to normal routing
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		r.reply(ctx, msg, "Usage: /approve <id> or /deny <id>")
		return true, nil
	}

	switch cmd {
	case "deny":
		if err := r.central.DeletePendingApproval(id); err != nil {
			return true, err
		}
		r.reply(ctx, msg, fmt.Sprintf("Denied request %d.", id))
		return true, nil
	case "approve":
		p, err := r.central.ApprovePendingApproval(id)
		if err != nil {
			return true, err
		}
		if p == nil {
			r.reply(ctx, msg, fmt.Sprintf("No pending request %d.", id))
			return true, nil
		}
		r.reply(ctx, msg, fmt.Sprintf("Approved %s. Replaying their message.", p.SenderID))
		// Replay the original message now that the sender is a known member.
		replay := channels.InboundMsg{
			Channel:  p.Channel,
			ChatID:   p.ChatID,
			SenderID: p.SenderID,
			Sender:   p.SenderName,
			Text:     p.Text,
		}
		if err := r.route(ctx, replay); err != nil {
			r.log.Error("replay approved message", "approval_id", id, "err", err)
		}
		return true, nil
	}
	return false, nil
}

// reply sends a short host message back to the conversation a command came from.
func (r *Router) reply(ctx context.Context, msg channels.InboundMsg, text string) {
	if r.sender == nil {
		return
	}
	if err := r.sender.Send(ctx, channels.OutboundMsg{
		Channel: msg.Channel, ChatID: msg.ChatID, Text: text,
	}); err != nil {
		r.log.Error("send reply", "channel", msg.Channel, "err", err)
	}
}

// parseApprovalCommand recognizes "/approve <id>" and "/deny <id>" (leading/
// trailing space tolerated). Returns (cmd, idArg, ok).
func parseApprovalCommand(text string) (cmd, idArg string, ok bool) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) != 2 {
		return "", "", false
	}
	switch fields[0] {
	case "/approve":
		return "approve", fields[1], true
	case "/deny":
		return "deny", fields[1], true
	}
	return "", "", false
}

// displayName prefers the sender's display name, falling back to the id.
func displayName(msg channels.InboundMsg) string {
	if msg.Sender != "" {
		return msg.Sender
	}
	return msg.SenderID
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
		// One container per agent group serves all its sessions, so launch
		// against the group dir (the parent of every session subdir), plus any
		// extra mounts the group requested (validated against the allowlist by
		// the ensurer).
		groupDir := db.AgentGroupDir(r.dataDir, agentGroupID)
		extra, err := r.extraMountRequests(agentGroupID)
		if err != nil {
			return err
		}
		if err := r.ensurer.EnsureRunner(ctx, agentGroupID, groupDir, extra...); err != nil {
			// Don't lose the message over a launch failure — it's safely queued
			// and a later message (or retry) can bring the runner up.
			r.log.Error("ensure runner failed (message remains queued)",
				"agent_group", agentGroupID, "session", sessionKey, "err", err)
		} else {
			r.log.Info("runner ensured", "agent_group", agentGroupID, "session", sessionKey)
		}
	}
	return nil
}

// extraMountRequests loads an agent group's requested extra mounts and converts
// them to mounts.Request for allowlist validation at launch (brief §6.3).
func (r *Router) extraMountRequests(agentGroupID int64) ([]mounts.Request, error) {
	ams, err := r.central.AgentMounts(agentGroupID)
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
