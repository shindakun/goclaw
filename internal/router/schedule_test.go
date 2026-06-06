package router

import (
	"strings"
	"testing"

	"github.com/shindakun/goclaw/internal/command"
	"github.com/shindakun/goclaw/internal/db"
)

// scheduleRouter seeds an owner + default group, wires a telegram conversation to it, and
// returns a router with a command registry. Returns the owner user id.
func scheduleRouter(t *testing.T) (*Router, int64) {
	t.Helper()
	d := testDB(t)
	ownerID, agID, err := d.Apply(db.Bootstrap{
		OwnerTelegramID:         "555",
		DefaultAgentGroupName:   "default",
		DefaultAgentGroupFolder: "default",
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	// Wire the conversation telegram:555 to the agent group (auto-wire on the owner's
	// first message does this; here wire directly so agentGroupFor resolves).
	mgID, err := d.UpsertMessagingGroup("telegram", "555", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnsureWiring(mgID, agID, "owner", "strict"); err != nil {
		t.Fatal(err)
	}
	r := New(d, t.TempDir(), 0, nil, nil, nil, command.NewRegistry(), quietLogger())
	return r, ownerID
}

// A reply that IS a /schedule directive is intercepted; a normal reply is passed through.
func TestIntercept_DetectsDirective(t *testing.T) {
	r, _ := scheduleRouter(t)

	if _, handled := r.Intercept("telegram", "555", "Here is your summary."); handled {
		t.Error("a normal reply should NOT be intercepted")
	}
	if _, handled := r.Intercept("telegram", "555", "I'll use /schedule later maybe"); handled {
		t.Error("a reply that merely mentions /schedule (not leading) should pass through")
	}
	if _, handled := r.Intercept("telegram", "555", "/schedule list"); !handled {
		t.Error("a leading /schedule directive should be intercepted")
	}
}

// Full add -> list -> remove through the intercept (the agent-emitted path), attributed
// to the conversation owner and targeting this conversation.
func TestIntercept_AddListRemove(t *testing.T) {
	r, ownerID := scheduleRouter(t)

	add, _ := r.Intercept("telegram", "555", "/schedule add inbox 8 Summarize my inbox.")
	if !strings.Contains(add, "Scheduled") || !strings.Contains(add, "08:00") {
		t.Fatalf("add reply = %q", add)
	}

	// The task exists, owned by the owner, targeting this conversation.
	tasks, err := r.central.ScheduledTasksByOwner(ownerID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("ScheduledTasksByOwner = %+v err=%v", tasks, err)
	}
	got := tasks[0]
	if got.Name != "inbox" || got.AtHour != 8 || got.Channel != "telegram" || got.ChatID != "555" {
		t.Fatalf("task = %+v, want inbox/8/telegram/555", got)
	}
	if got.Prompt != "Summarize my inbox." {
		t.Fatalf("prompt = %q", got.Prompt)
	}

	list, _ := r.Intercept("telegram", "555", "/schedule list")
	if !strings.Contains(list, "inbox") {
		t.Fatalf("list reply = %q", list)
	}

	rm, _ := r.Intercept("telegram", "555", "/schedule remove inbox")
	if !strings.Contains(rm, "Removed") {
		t.Fatalf("remove reply = %q", rm)
	}
	if tasks, _ := r.central.ScheduledTasksByOwner(ownerID); len(tasks) != 0 {
		t.Fatalf("task not removed: %+v", tasks)
	}
}

// Guards: a bad hour and an unwired conversation are rejected with a message, not a panic.
func TestIntercept_Guards(t *testing.T) {
	r, _ := scheduleRouter(t)

	bad, _ := r.Intercept("telegram", "555", "/schedule add x 99 do it")
	if !strings.Contains(bad, "0-23") {
		t.Fatalf("bad hour reply = %q", bad)
	}
	// An unwired conversation (telegram:999) cannot schedule.
	unwired, _ := r.Intercept("telegram", "999", "/schedule add x 8 do it")
	if !strings.Contains(unwired, "not wired") {
		t.Fatalf("unwired reply = %q", unwired)
	}
}

// HH:MM is honored exactly: "07:30" stores hour 7, minute 30, and the confirmation reads
// 07:30, never 07:00. This is the regression for the "7:30 silently became 7:00" bug.
func TestIntercept_AddHHMM(t *testing.T) {
	r, ownerID := scheduleRouter(t)

	add, _ := r.Intercept("telegram", "555", "/schedule add inbox 07:30 Summarize my inbox.")
	if !strings.Contains(add, "07:30") {
		t.Fatalf("add reply should confirm 07:30, got %q", add)
	}
	tasks, err := r.central.ScheduledTasksByOwner(ownerID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("ScheduledTasksByOwner = %+v err=%v", tasks, err)
	}
	if tasks[0].AtHour != 7 || tasks[0].AtMinute != 30 {
		t.Fatalf("stored time = %02d:%02d, want 07:30", tasks[0].AtHour, tasks[0].AtMinute)
	}
}

// A malformed or out-of-range time is REJECTED with a message and stores NOTHING; it is
// never rounded to a representable value. "bare hour" still works (8 -> 08:00).
func TestIntercept_TimeParsingAndRejection(t *testing.T) {
	r, ownerID := scheduleRouter(t)

	// A bare hour is accepted as HH:00 (back-compat).
	bare, _ := r.Intercept("telegram", "555", "/schedule add a 8 do it")
	if !strings.Contains(bare, "08:00") {
		t.Fatalf("bare hour reply = %q", bare)
	}

	// Out-of-range minute is rejected, not rounded.
	badMin, _ := r.Intercept("telegram", "555", "/schedule add b 7:75 do it")
	if !strings.Contains(badMin, "00-59") {
		t.Fatalf("bad-minute reply = %q (want a 00-59 rejection, never a stored 08:15)", badMin)
	}
	// Non-numeric time is rejected.
	garbage, _ := r.Intercept("telegram", "555", "/schedule add c noon do it")
	if !strings.Contains(garbage, "0-23") {
		t.Fatalf("garbage-time reply = %q", garbage)
	}

	// Only the valid one was stored; the two rejects created nothing.
	tasks, _ := r.central.ScheduledTasksByOwner(ownerID)
	if len(tasks) != 1 || tasks[0].Name != "a" {
		t.Fatalf("rejected times must not be stored; tasks = %+v", tasks)
	}
}

// The /schedule COMMAND path (user-typed) goes through the same logic, owner-scoped by
// req.UserID.
func TestCmdSchedule_UserTyped(t *testing.T) {
	r, ownerID := scheduleRouter(t)
	reply, err := r.cmdSchedule(nil, command.Request{
		UserID: ownerID, Channel: "telegram", ChatID: "555",
		Args: "add daily 9 Do the thing.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Scheduled") {
		t.Fatalf("cmd reply = %q", reply)
	}
	if tasks, _ := r.central.ScheduledTasksByOwner(ownerID); len(tasks) != 1 {
		t.Fatalf("task not created via command: %+v", tasks)
	}
	// Unknown user is refused.
	if reply, _ := r.cmdSchedule(nil, command.Request{UserID: 0, Args: "list"}); !strings.Contains(reply, "known user") {
		t.Fatalf("unknown-user reply = %q", reply)
	}
}
