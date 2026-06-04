package router

import (
	"context"
	"strings"
	"testing"

	"github.com/shindakun/goclaw/internal/channels"
	"github.com/shindakun/goclaw/internal/command"
	"github.com/shindakun/goclaw/internal/db"
	"github.com/shindakun/goclaw/internal/permissions"
)

// cmdSetup seeds an owner and returns a router with a fake sender so command
// replies can be inspected.
func cmdSetup(t *testing.T) (*Router, *db.DB, *fakeSender) {
	t.Helper()
	d := testDB(t)
	if _, _, err := d.Apply(db.Bootstrap{
		OwnerTelegramID:         "1000",
		DefaultAgentGroupName:   "default",
		DefaultAgentGroupFolder: "default",
	}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	fs := &fakeSender{}
	r := New(d, t.TempDir(), 0, nil, fs, nil, nil, quietLogger())
	return r, d, fs
}

// /commands from the owner lists the built-ins, and the listing includes the
// owner-only /approve (since the owner may see it).
func TestCommand_OwnerSeesCommandsListing(t *testing.T) {
	r, _, fs := cmdSetup(t)
	owner, err := r.central.UserByIdentity("telegram", "1000")
	if err != nil || owner == nil {
		t.Fatalf("owner lookup: %v", err)
	}

	handled, err := r.handleCommand(context.Background(),
		channels.InboundMsg{Channel: "telegram", ChatID: "1000", SenderID: "1000", Text: "/commands"},
		owner)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if len(fs.sent) != 1 {
		t.Fatalf("expected one listing reply, got %d", len(fs.sent))
	}
	out := fs.sent[0].Text
	for _, want := range []string{"/commands", "/reset", "/compact", "/approve"} {
		if !strings.Contains(out, want) {
			t.Fatalf("owner listing missing %q:\n%s", want, out)
		}
	}
}

// Regression: a host-executed command (/commands) routed through the FULL route()
// path is answered by the host and must NOT be enqueued to the agent's session
// inbound. (The bug: a stale host let /commands reach the container, which listed
// its own skills.)
func TestCommand_NotEnqueuedToAgent(t *testing.T) {
	r, d, fs := cmdSetup(t)
	// Wire the owner's conversation so an enqueue path exists to rule out.
	mgID, err := d.UpsertMessagingGroup("telegram", "1000", "")
	if err != nil {
		t.Fatalf("mg: %v", err)
	}
	_, agID, err := d.Apply(db.Bootstrap{DefaultAgentGroupName: "default", DefaultAgentGroupFolder: "default"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := d.EnsureWiring(mgID, agID, string(permissions.ScopeAll), string(permissions.PolicyStrict)); err != nil {
		t.Fatalf("wiring: %v", err)
	}

	if err := r.route(context.Background(), channels.InboundMsg{
		Channel: "telegram", ChatID: "1000", SenderID: "1000", Sender: "owner", Text: "/commands",
	}); err != nil {
		t.Fatalf("route: %v", err)
	}

	// The host replied with the listing.
	if len(fs.sent) != 1 || !strings.Contains(fs.sent[0].Text, "/commands") {
		t.Fatalf("expected a host listing reply, got %+v", fs.sent)
	}
	// And nothing was enqueued for the agent.
	sess, err := db.OpenSession(r.dataDir, agID, "telegram:1000")
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() { _ = sess.Close() }()
	if in, _ := sess.PendingInbound(); len(in) != 0 {
		t.Fatalf("/commands must not be enqueued to the agent, got %+v", in)
	}
}

// A pass-through command (/reset) is NOT handled by the host: it falls through to
// normal routing so the agent runner receives it.
func TestCommand_PassThroughFallsThrough(t *testing.T) {
	r, _, _ := cmdSetup(t)
	owner, _ := r.central.UserByIdentity("telegram", "1000")
	handled, err := r.handleCommand(context.Background(),
		channels.InboundMsg{Channel: "telegram", ChatID: "1000", SenderID: "1000", Text: "/reset"},
		owner)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if handled {
		t.Fatal("/reset is pass-through and must NOT be handled by the host")
	}
}

// A member issuing an owner-only command is not handled (falls through), and does
// not leak the command's existence.
func TestCommand_MemberDeniedOwnerCommand(t *testing.T) {
	r, d, _ := cmdSetup(t)
	// Seed a plain member.
	if _, err := d.UpsertUserWithIdentity("mem", string(permissions.RoleMember), "telegram", "222"); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	member, _ := d.UserByIdentity("telegram", "222")

	handled, err := r.handleCommand(context.Background(),
		channels.InboundMsg{Channel: "telegram", ChatID: "555", SenderID: "222", Text: "/approve 1"},
		member)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if handled {
		t.Fatal("member /approve should fall through, not be handled")
	}
}

// A registered plugin-style command runs its handler and the reply is sent.
func TestCommand_PluginCommandRuns(t *testing.T) {
	r, _, fs := cmdSetup(t)
	owner, _ := r.central.UserByIdentity("telegram", "1000")

	var gotArgs string
	r.Commands().Register(command.Command{
		Name:        "roll",
		Description: "Roll dice.",
		Source:      "roll",
		Handler: func(_ context.Context, req command.Request) (string, error) {
			gotArgs = req.Args
			return "rolled: " + req.Args, nil
		},
	})

	handled, err := r.handleCommand(context.Background(),
		channels.InboundMsg{Channel: "telegram", ChatID: "1000", SenderID: "1000", Text: "/roll 2d6"},
		owner)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if gotArgs != "2d6" {
		t.Fatalf("handler got args %q, want 2d6", gotArgs)
	}
	if len(fs.sent) != 1 || fs.sent[0].Text != "rolled: 2d6" {
		t.Fatalf("plugin command reply wrong: %+v", fs.sent)
	}
}
