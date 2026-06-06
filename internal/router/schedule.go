package router

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/shindakun/goclaw/internal/command"
	"github.com/shindakun/goclaw/internal/db"
	"github.com/shindakun/goclaw/internal/permissions"
)

// maxTasksPerOwner caps how many scheduled tasks one owner may have, so a runaway
// "schedule everything" cannot fill the table or hammer the agent.
const maxTasksPerOwner = 25

// registerScheduleCommand adds the owner-gated /schedule command. Called from New (with
// the other built-in registrations) when the registry is present.
func (r *Router) registerScheduleCommand() {
	if r.commands == nil {
		return
	}
	r.commands.Register(command.Command{
		Name:        "schedule",
		Description: "Recurring tasks: /schedule add <name> <hour> <prompt> | list | remove <name> | pause <name> | resume <name>",
		MinRole:     permissions.RoleOwner,
		Source:      "builtin",
		Handler:     r.cmdSchedule,
	})
}

// cmdSchedule handles "/schedule <sub> ..." typed by a user. Owner-only (MinRole). Tasks
// are scoped to the calling user; the target defaults to THIS conversation.
func (r *Router) cmdSchedule(ctx context.Context, req command.Request) (string, error) {
	if req.UserID == 0 {
		return "Scheduling needs a known user.", nil
	}
	return r.runSchedule(req.UserID, req.Channel, req.ChatID, req.Args)
}

// runSchedule executes a "<sub> ..." schedule directive for ownerID, targeting the given
// conversation. Shared by the /schedule command (user-typed) and the outbound intercept
// (agent-emitted), so both go through the same owner-scoped logic.
func (r *Router) runSchedule(ownerID int64, channel, chatID, args string) (string, error) {
	sub, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
	rest = strings.TrimSpace(rest)
	switch strings.ToLower(sub) {
	case "list", "":
		return r.scheduleList(ownerID)
	case "add":
		return r.scheduleAdd(ownerID, channel, chatID, rest)
	case "remove", "rm", "delete":
		return r.scheduleSetOrRemove(ownerID, rest, "remove")
	case "pause", "disable":
		return r.scheduleSetOrRemove(ownerID, rest, "pause")
	case "resume", "enable":
		return r.scheduleSetOrRemove(ownerID, rest, "resume")
	default:
		return "Usage: /schedule add <name> <hour 0-23> <prompt> | list | remove <name> | pause <name> | resume <name>", nil
	}
}

// Intercept implements delivery.OutboundInterceptor: if the agent's reply IS a
// "/schedule ..." directive, run it against the DB (attributed to this conversation's
// owner, targeting this conversation) and return its result to send instead of the raw
// command text. Anything else is passed through untouched (handled=false).
//
// This is how natural-language scheduling works without a runner->host tool RPC: the
// agent, told it can schedule by emitting a /schedule line, produces one; the host
// executes it here, the same logic the /schedule command uses. The agent must emit the
// directive as the WHOLE reply (a leading "/schedule") so we never misfire on a reply
// that merely mentions it.
func (r *Router) Intercept(channel, chatID, text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	const prefix = "/schedule"
	if trimmed != prefix && !strings.HasPrefix(trimmed, prefix+" ") {
		return "", false
	}
	args := strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))

	ownerID, found, err := r.central.OwnerUserID()
	if err != nil {
		r.log.Error("schedule intercept: owner lookup", "err", err)
		return "Could not schedule (owner lookup failed).", true
	}
	if !found {
		return "Could not schedule (no owner configured).", true
	}
	reply, err := r.runSchedule(ownerID, channel, chatID, args)
	if err != nil {
		r.log.Error("schedule intercept: run", "err", err)
		return "Could not complete the schedule request.", true
	}
	return reply, true
}

// scheduleAdd parses "<name> <hour> <prompt...>" and creates a daily task targeting the
// given conversation. Hour is a local 0-23 wall-clock hour.
func (r *Router) scheduleAdd(ownerID int64, channel, chatID, rest string) (string, error) {
	name, r2, ok := strings.Cut(rest, " ")
	if !ok {
		return "Usage: /schedule add <name> <hour 0-23> <prompt>", nil
	}
	hourStr, prompt, ok := strings.Cut(strings.TrimSpace(r2), " ")
	prompt = strings.TrimSpace(prompt)
	if !ok || prompt == "" {
		return "Usage: /schedule add <name> <hour 0-23> <prompt>", nil
	}
	name = strings.TrimSpace(name)
	hour, err := strconv.Atoi(strings.TrimSpace(hourStr))
	if err != nil || hour < 0 || hour > 23 {
		return "The hour must be 0-23 (local). Usage: /schedule add <name> <hour> <prompt>", nil
	}

	// Per-owner cap (fail closed on too many).
	if n, err := r.central.CountScheduledTasksByOwner(ownerID); err != nil {
		return "", err
	} else if n >= maxTasksPerOwner {
		return fmt.Sprintf("You already have %d scheduled tasks (the max). Remove one first.", n), nil
	}

	// Target = THIS conversation: the agent group it is wired to, and a session keyed
	// like a normal message, so the summary is delivered back here.
	agentGroupID, err := r.agentGroupFor(channel, chatID)
	if err != nil {
		return "", err
	}
	if agentGroupID == 0 {
		return "This conversation is not wired to an agent group; cannot schedule here.", nil
	}
	sessionKey := channel + ":" + chatID

	_, err = r.central.CreateScheduledTask(db.ScheduledTask{
		Name:         name,
		OwnerUserID:  ownerID,
		AgentGroupID: agentGroupID,
		SessionKey:   sessionKey,
		Channel:      channel,
		ChatID:       chatID,
		PeriodDays:   1,
		AtHour:       hour,
		Prompt:       prompt,
		Enabled:      true,
	})
	if err != nil {
		if err == db.ErrTaskExists {
			return fmt.Sprintf("You already have a task named %q. Remove it first or pick another name.", name), nil
		}
		return "", err
	}
	return fmt.Sprintf("Scheduled %q daily at %02d:00. I'll run it and reply here.", name, hour), nil
}

func (r *Router) scheduleList(ownerID int64) (string, error) {
	tasks, err := r.central.ScheduledTasksByOwner(ownerID)
	if err != nil {
		return "", err
	}
	if len(tasks) == 0 {
		return "No scheduled tasks. Add one: /schedule add <name> <hour> <prompt>", nil
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Name < tasks[j].Name })
	var b strings.Builder
	b.WriteString("Scheduled tasks:\n")
	for _, t := range tasks {
		when := fmt.Sprintf("daily %02d:00", t.AtHour)
		if t.AtHour < 0 {
			when = fmt.Sprintf("every %ds", t.EverySeconds)
		}
		status := ""
		if !t.Enabled {
			status = " (paused)"
		}
		fmt.Fprintf(&b, "  %s (%s%s): %s\n", t.Name, when, status, t.Prompt)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// scheduleSetOrRemove handles remove/pause/resume by name, owner-scoped.
func (r *Router) scheduleSetOrRemove(ownerID int64, name, action string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Sprintf("Usage: /schedule %s <name>", action), nil
	}
	switch action {
	case "remove":
		ok, err := r.central.DeleteScheduledTaskByName(ownerID, name)
		if err != nil {
			return "", err
		}
		if !ok {
			return fmt.Sprintf("No task named %q.", name), nil
		}
		return fmt.Sprintf("Removed %q.", name), nil
	case "pause", "resume":
		enabled := action == "resume"
		ok, err := r.central.SetScheduledTaskEnabledByName(ownerID, name, enabled)
		if err != nil {
			return "", err
		}
		if !ok {
			return fmt.Sprintf("No task named %q.", name), nil
		}
		if enabled {
			return fmt.Sprintf("Resumed %q.", name), nil
		}
		return fmt.Sprintf("Paused %q (kept, will not fire until resumed).", name), nil
	}
	return "", fmt.Errorf("schedule: unknown action %q", action)
}

// agentGroupFor resolves the agent group a conversation is wired to (0 if unwired).
func (r *Router) agentGroupFor(channel, chatID string) (int64, error) {
	mgID, err := r.central.UpsertMessagingGroup(channel, chatID, "")
	if err != nil {
		return 0, err
	}
	wiring, err := r.central.WiringForMessagingGroup(mgID)
	if err != nil {
		return 0, err
	}
	if wiring == nil {
		return 0, nil
	}
	return wiring.AgentGroupID, nil
}
