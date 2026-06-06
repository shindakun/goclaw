// Package router resolves an inbound message's routing chain
// (user → messaging group → agent group → session), applies the access gate,
// and writes the message into the session's inbound.db (brief §3.2, §5).
//
// The router is pure host-side logic: it reads the central DB to resolve
// entities and the permissions package to gate access, then enqueues into
// inbound.db. It does not implement container orchestration itself - after a
// successful enqueue it asks an injected RunnerEnsurer to make sure a runner is
// up for that session, keeping the Podman dependency out of this package.
package router

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shindakun/goclaw/internal/channels"
	"github.com/shindakun/goclaw/internal/command"
	"github.com/shindakun/goclaw/internal/db"
	"github.com/shindakun/goclaw/internal/mounts"
	"github.com/shindakun/goclaw/internal/permissions"
	"github.com/shindakun/goclaw/internal/plugin"
)

// RunnerEnsurer makes sure a runner is up for an agent group after a message is
// enqueued. One container per agent group serves all its sessions, plus any
// extra mounts the group requested (validated against the allowlist by the
// implementation). internal/runtime implements this; it may be nil (no
// orchestration, e.g. when running the stub runner by hand or in tests).
type RunnerEnsurer interface {
	EnsureRunner(ctx context.Context, agentGroupID int64, groupDir string, extra ...mounts.Request) error
}

// Sender sends a host-originated reply on a channel - used for the approval-card
// flow (notifying the owner, confirming approve/deny). internal/channels.Registry
// satisfies this. May be nil to disable host-sent messages.
type Sender interface {
	Send(ctx context.Context, out channels.OutboundMsg) error
}

// Typer shows a "typing…" indicator for a chat until Stop. internal/typing
// implements it; may be nil to disable the indicator. Start is called when an
// allowed message is enqueued; the delivery loop calls Stop after replying.
type Typer interface {
	Start(ctx context.Context, channel, chatID string)
}

// Router routes inbound messages to session inbound DBs.
type Router struct {
	central   *db.DB
	dataDir   string
	ensurer   RunnerEnsurer
	sender    Sender
	typer     Typer
	commands  *command.Registry
	installer *plugin.Installer // drives /plugin add|remove|list; nil disables /plugin
	pluginDir string            // host plugins dir (for re-listing commands after add/remove)
	log       *slog.Logger

	// autoWireAgentGroupID, when non-zero, lets the owner bootstrap a wiring by
	// simply messaging the host: an owner message in an unwired conversation
	// auto-wires that conversation to this agent group (scope=owner). Off (0)
	// unless GOCLAW_AUTO_WIRE_OWNER is set. This is a convenience for first-run;
	// real deployments wire explicitly.
	autoWireAgentGroupID int64
}

// New constructs a Router over the central DB. autoWireAgentGroupID may be 0 to
// disable owner auto-wiring; ensurer may be nil to disable container launch;
// sender may be nil to disable host-sent messages (the approval-card flow);
// typer may be nil to disable the typing indicator.
func New(central *db.DB, dataDir string, autoWireAgentGroupID int64, ensurer RunnerEnsurer, sender Sender, typer Typer, commands *command.Registry, log *slog.Logger) *Router {
	if commands == nil {
		commands = command.NewRegistry()
	}
	r := &Router{
		central:              central,
		dataDir:              dataDir,
		ensurer:              ensurer,
		sender:               sender,
		typer:                typer,
		commands:             commands,
		log:                  log,
		autoWireAgentGroupID: autoWireAgentGroupID,
	}
	r.registerBuiltinCommands()
	return r
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

	// Slash commands the HOST executes are handled before normal routing. A
	// pass-through command (e.g. /reset, /compact) or an unknown slash falls
	// through to normal routing, so the agent runner still receives it.
	if handled, err := r.handleCommand(ctx, msg, user); err != nil {
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
		// No reachable owner (or no sender wired) - the request is still held in
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

// registerBuiltinCommands adds the host's built-in commands to the registry: the
// /commands and /help listing, the /reset and /compact pass-through entries, and
// the owner/admin approval commands. Plugin commands register separately (the
// plugin manager calls r.Commands().Register).
func (r *Router) registerBuiltinCommands() {
	command.RegisterListing(r.commands) // /commands, /help, /reset, /compact
	r.commands.Register(command.Command{
		Name:        "approve",
		Description: "Approve a pending access request: /approve <id>",
		MinRole:     permissions.RoleAdmin,
		Source:      "builtin",
		Handler:     r.cmdApprove,
	})
	r.commands.Register(command.Command{
		Name:        "deny",
		Description: "Deny a pending access request: /deny <id>",
		MinRole:     permissions.RoleAdmin,
		Source:      "builtin",
		Handler:     r.cmdDeny,
	})
	r.registerScheduleCommand()
}

// Commands exposes the registry so the host (e.g. the plugin manager) can register
// or remove plugin-provided commands at runtime.
func (r *Router) Commands() *command.Registry { return r.commands }

// SetInstaller wires the plugin installer and registers the owner-only `/plugin`
// command (add/list/remove). pluginDir is the host plugins directory, used to
// refresh the /commands listing after an install or removal. Call once at startup.
func (r *Router) SetInstaller(in *plugin.Installer, pluginDir string) {
	r.installer = in
	r.pluginDir = pluginDir
	r.commands.Register(command.Command{
		Name:        "plugin",
		Description: "Manage plugins: /plugin add <git-url> | list | remove <name>",
		MinRole:     permissions.RoleOwner,
		Source:      "builtin",
		Handler:     r.cmdPlugin,
	})
}

// cmdPlugin handles "/plugin <add|list|remove> ...". Owner-only (enforced by the
// command's MinRole). Installs build the plugin inside a sandbox container; the
// in-container runner's watch loads/unloads the result, so there is no restart.
func (r *Router) cmdPlugin(ctx context.Context, req command.Request) (string, error) {
	if r.installer == nil {
		return "Plugin management is unavailable (no runner configured).", nil
	}
	sub, arg, _ := strings.Cut(strings.TrimSpace(req.Args), " ")
	arg = strings.TrimSpace(arg)
	switch strings.ToLower(sub) {
	case "list", "":
		return r.pluginList()
	case "add":
		if arg == "" {
			return "Usage: /plugin add <git-url>[#<subdir>]  (subdir selects one plugin in a monorepo, e.g. ...#cmd/gmail)", nil
		}
		return r.pluginAdd(ctx, arg)
	case "remove", "rm":
		if arg == "" {
			return "Usage: /plugin remove <name>", nil
		}
		return r.pluginRemove(arg)
	default:
		return "Unknown subcommand. Use: /plugin add <git-url>[#<subdir>] | list | remove <name>", nil
	}
}

func (r *Router) pluginList() (string, error) {
	mans, err := r.installer.List()
	if err != nil {
		return "", err
	}
	if len(mans) == 0 {
		return "No plugins installed. Add one with /plugin add <git-url>.", nil
	}
	var b strings.Builder
	b.WriteString("Installed plugins:\n")
	for _, m := range mans {
		fmt.Fprintf(&b, "  %s v%s", m.Name, m.Version)
		if m.Command != "" {
			fmt.Fprintf(&b, " (/%s)", m.Command)
		}
		if m.Author != "" {
			fmt.Fprintf(&b, " by %s", m.Author)
		}
		if m.Description != "" {
			fmt.Fprintf(&b, " - %s", m.Description)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func (r *Router) pluginAdd(ctx context.Context, gitURL string) (string, error) {
	res, err := r.installer.Add(ctx, gitURL)
	if err != nil {
		return "Install failed: " + err.Error(), nil
	}
	// Refresh the host's /commands listing so the new plugin's command shows up.
	r.RegisterPluginCommands(r.pluginDir)
	msg := fmt.Sprintf("Installed %s v%s", res.Name, res.Version)
	if res.Command != "" {
		msg += fmt.Sprintf(" (/%s)", res.Command)
	}
	if res.Commit != "" {
		msg += " at " + shortCommit(res.Commit)
	}
	msg += ". It will be live within a few seconds."
	return msg, nil
}

func (r *Router) pluginRemove(name string) (string, error) {
	removed, err := r.installer.Remove(name)
	if err != nil {
		return "Remove failed: " + err.Error(), nil
	}
	if !removed {
		return fmt.Sprintf("No plugin named %q is installed.", name), nil
	}
	r.commands.UnregisterSource(name) // drop its /commands listing
	return fmt.Sprintf("Removed plugin %q.", name), nil
}

func shortCommit(c string) string {
	if len(c) > 8 {
		return c[:8]
	}
	return c
}

// RegisterPluginCommands reads each plugin.yml under pluginsDir and registers any
// declared slash command as a PASS-THROUGH listing, so /commands shows it. The host
// does NOT execute plugin commands: plugins run in the agent container, and the
// in-container runner intercepts the command (the host lets it route inward, exactly
// like /reset and /compact). This only makes plugin commands DISCOVERABLE from the
// host; it does not launch anything. A bad manifest is skipped, never fatal.
func (r *Router) RegisterPluginCommands(pluginsDir string) {
	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		return // no plugins dir is fine
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue // skip non-dirs and hidden dirs (e.g. an in-progress .build-*)
		}
		man, err := plugin.LoadManifest(filepath.Join(pluginsDir, e.Name()))
		if err != nil || man.Command == "" {
			continue
		}
		r.commands.Register(command.Command{
			Name:        man.Command,
			Description: man.Description,
			Source:      man.Name,
			PassThrough: true, // host does not run it; the in-container runner does
		})
	}
}

// handleCommand dispatches a host-executed slash command. It returns (handled,
// err): handled is true only when the host consumed the message. A pass-through
// command, an unknown command, or a command the sender's role may not run all
// return (false, nil) so the message falls through to normal routing (and, for
// pass-through, reaches the agent runner).
func (r *Router) handleCommand(ctx context.Context, msg channels.InboundMsg, user *db.User) (bool, error) {
	name, args, ok := command.IsCommand(msg.Text)
	if !ok {
		return false, nil
	}
	cmd, found := r.commands.Get(name)
	if !found || cmd.PassThrough || cmd.Handler == nil {
		return false, nil // unknown or pass-through: let normal routing have it
	}

	role := permissions.RoleMember
	known := user != nil
	if user != nil {
		role = permissions.Role(user.Role)
	}
	// Role gate: a sender below the command's MinRole is treated as if the command
	// does not exist (it falls through to normal routing rather than leaking that
	// the command exists). Visibility in /commands already hides it from them.
	if !known || !roleAtLeast(role, cmd.MinRole) {
		return false, nil
	}

	req := command.Request{
		Channel:  msg.Channel,
		ChatID:   msg.ChatID,
		SenderID: msg.SenderID,
		Sender:   msg.Sender,
		Role:     role,
		IsKnown:  known,
		Args:     args,
	}
	if user != nil {
		req.UserID = user.ID
	}
	reply, err := cmd.Handler(ctx, req)
	if err != nil {
		return true, err
	}
	if reply != "" {
		r.reply(ctx, msg, reply)
	}
	return true, nil
}

// cmdDeny handles "/deny <id>".
func (r *Router) cmdDeny(_ context.Context, req command.Request) (string, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(req.Args), 10, 64)
	if err != nil {
		return "Usage: /deny <id>", nil
	}
	if err := r.central.DeletePendingApproval(id); err != nil {
		return "", err
	}
	return fmt.Sprintf("Denied request %d.", id), nil
}

// cmdApprove handles "/approve <id>": it makes the held sender a known member and
// replays their original message through routing.
func (r *Router) cmdApprove(ctx context.Context, req command.Request) (string, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(req.Args), 10, 64)
	if err != nil {
		return "Usage: /approve <id>", nil
	}
	p, err := r.central.ApprovePendingApproval(id)
	if err != nil {
		return "", err
	}
	if p == nil {
		return fmt.Sprintf("No pending request %d.", id), nil
	}
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
	return fmt.Sprintf("Approved %s. Replaying their message.", p.SenderID), nil
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

// roleAtLeast reports whether have meets or exceeds need (owner > admin > member).
func roleAtLeast(have, need permissions.Role) bool {
	rank := func(role permissions.Role) int {
		switch role {
		case permissions.RoleOwner:
			return 3
		case permissions.RoleAdmin:
			return 2
		case permissions.RoleMember:
			return 1
		default:
			return 0
		}
	}
	return rank(have) >= rank(need)
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

	// Write the message into inbound.db with a verified, retried enqueue. The
	// session DB is shared with the container across a bind mount, where a
	// concurrent writer can lose a committed insert; EnqueueInbound reads the row
	// back, and we retry a few times before giving up. We only proceed (and log
	// success) once the write is confirmed durable.
	id, err := r.enqueueDurable(agentGroupID, sessionKey, msg)
	if err != nil {
		// The message was NOT accepted. Say so plainly, and tell the user so they
		// can resend rather than silently believing it was received.
		r.log.Error("message NOT enqueued (write not durable)",
			"channel", msg.Channel, "agent_group", agentGroupID, "session", sessionKey, "err", err)
		r.notifyDropped(ctx, msg)
		return err
	}
	r.log.Info("message enqueued to inbound",
		"channel", msg.Channel, "agent_group", agentGroupID, "session", sessionKey, "msg_id", id)

	// Show a "typing…" indicator while the runner works; the delivery loop stops
	// it once the reply is sent (best-effort; no-op if the channel can't type).
	if r.typer != nil {
		r.typer.Start(ctx, msg.Channel, msg.ChatID)
	}

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
			// Don't lose the message over a launch failure - it's safely queued
			// and a later message (or retry) can bring the runner up.
			r.log.Error("ensure runner failed (message remains queued)",
				"agent_group", agentGroupID, "session", sessionKey, "err", err)
		} else {
			r.log.Info("runner ensured", "agent_group", agentGroupID, "session", sessionKey)
		}
	}
	return nil
}

// enqueueInboundAttempts is how many times enqueueDurable retries a verified
// inbound write before declaring the message lost.
const enqueueInboundAttempts = 4

// enqueueDurable writes the message into the session inbound and confirms it
// persisted, retrying on a non-durable write (the session DB is shared with the
// container across a bind mount, where a concurrent writer can clobber an
// insert). Returns the row id only on a verified write; otherwise the final
// error so the caller can tell the user the message was not accepted.
func (r *Router) enqueueDurable(agentGroupID int64, sessionKey string, msg channels.InboundMsg) (int64, error) {
	var lastErr error
	for attempt := 1; attempt <= enqueueInboundAttempts; attempt++ {
		// Re-open per attempt so each retry gets a fresh handle/view of the file.
		sess, err := db.OpenSession(r.dataDir, agentGroupID, sessionKey)
		if err != nil {
			lastErr = err
			continue
		}
		id, err := sess.EnqueueInbound(msg.Channel, msg.ChatID, msg.SenderID, msg.Sender, msg.Text)
		_ = sess.Close()
		if err == nil {
			return id, nil
		}
		lastErr = err
		r.log.Warn("inbound write not durable, retrying",
			"session", sessionKey, "attempt", attempt, "err", err)
		time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
	}
	return 0, fmt.Errorf("inbound write failed after %d attempts: %w", enqueueInboundAttempts, lastErr)
}

// notifyDropped tells the user their message was not accepted, so they can
// resend rather than assume it was received. Best-effort; needs a sender.
func (r *Router) notifyDropped(ctx context.Context, msg channels.InboundMsg) {
	if r.sender == nil {
		return
	}
	_ = r.sender.Send(ctx, channels.OutboundMsg{
		Channel: msg.Channel,
		ChatID:  msg.ChatID,
		Text:    "⚠️ I couldn't save your message (a storage write failed), so it was NOT received. Please send it again.",
	})
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
