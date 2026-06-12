package router

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shindakun/goclaw/internal/channels"
	"github.com/shindakun/goclaw/internal/command"
	"github.com/shindakun/goclaw/internal/db"
	"github.com/shindakun/goclaw/internal/permissions"
	"github.com/shindakun/goclaw/internal/plugin"
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

// RegisterPluginCommands lists a plugin's command in /commands as a pass-through
// (host does not execute it; the in-container runner does).
func TestRegisterPluginCommands_ListsAsPassThrough(t *testing.T) {
	r, _, fs := cmdSetup(t)

	// Stage a plugin dir with a plugin.yml declaring a /roll command.
	dir := t.TempDir()
	pdir := dir + "/roll"
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	yml := "name: roll\nkind: tool\nversion: \"1.0.0\"\nexec: roll\n" +
		"description: Roll dice.\ncommand: roll\n"
	if err := os.WriteFile(pdir+"/plugin.yml", []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}

	r.RegisterPluginCommands(dir)

	c, ok := r.Commands().Get("roll")
	if !ok || !c.PassThrough {
		t.Fatalf("/roll should be a pass-through listing, got %+v ok=%v", c, ok)
	}

	// It shows in /commands for the owner.
	owner, _ := r.central.UserByIdentity("telegram", "1000")
	if _, err := r.handleCommand(context.Background(),
		channels.InboundMsg{Channel: "telegram", ChatID: "1000", SenderID: "1000", Text: "/commands"},
		owner); err != nil {
		t.Fatalf("commands: %v", err)
	}
	if len(fs.sent) == 0 || !strings.Contains(fs.sent[len(fs.sent)-1].Text, "/roll") {
		t.Fatalf("/commands listing should include /roll: %v", fs.sent)
	}

	// And the host does NOT execute it: /roll falls through (PassThrough), so the
	// message routes inward to the runner.
	handled, err := r.handleCommand(context.Background(),
		channels.InboundMsg{Channel: "telegram", ChatID: "1000", SenderID: "1000", Text: "/roll 2d6"},
		owner)
	if err != nil {
		t.Fatalf("roll: %v", err)
	}
	if handled {
		t.Fatal("/roll is pass-through; the host must NOT handle it")
	}
}

// /plugin list and remove are filesystem ops (no container), so they test end to
// end against a temp plugins dir. /plugin add needs a build container, covered
// separately.
func TestCmdPlugin_ListAndRemove(t *testing.T) {
	r, _, fs := cmdSetup(t)
	owner, _ := r.central.UserByIdentity("telegram", "1000")

	// Stage a fake installed plugin.
	pdir := t.TempDir()
	rolldir := filepath.Join(pdir, "roll")
	if err := os.MkdirAll(rolldir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rolldir, "roll"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	yml := "name: roll\nkind: tool\nversion: \"1.0.0\"\nexec: roll\n" +
		"author: shindakun\ndescription: Roll dice.\ncommand: roll\n"
	if err := os.WriteFile(filepath.Join(rolldir, "plugin.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}

	r.SetInstaller(plugin.NewInstaller(pdir, "img", "podman"), pdir)

	// /plugin list shows roll.
	send := func(text string) string {
		fs.sent = nil
		if _, err := r.handleCommand(context.Background(),
			channels.InboundMsg{Channel: "telegram", ChatID: "1000", SenderID: "1000", Text: text},
			owner); err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		if len(fs.sent) == 0 {
			t.Fatalf("%s: no reply", text)
		}
		return fs.sent[len(fs.sent)-1].Text
	}

	if out := send("/plugin list"); !strings.Contains(out, "roll") || !strings.Contains(out, "/roll") {
		t.Fatalf("list missing roll: %q", out)
	}

	// /plugin remove roll deletes it.
	if out := send("/plugin remove roll"); !strings.Contains(out, "Removed") {
		t.Fatalf("remove reply: %q", out)
	}
	if _, err := os.Stat(rolldir); !os.IsNotExist(err) {
		t.Fatal("roll dir should be deleted after remove")
	}
	if out := send("/plugin list"); !strings.Contains(out, "No plugins") {
		t.Fatalf("list after remove: %q", out)
	}

	// Usage messages.
	if out := send("/plugin add"); !strings.Contains(out, "Usage") {
		t.Fatalf("add usage: %q", out)
	}
	if out := send("/plugin bogus"); !strings.Contains(out, "Unknown subcommand") {
		t.Fatalf("bogus: %q", out)
	}
}

// A non-owner cannot use /plugin (owner-only MinRole): it falls through.
func TestCmdPlugin_OwnerOnly(t *testing.T) {
	r, d, _ := cmdSetup(t)
	r.SetInstaller(plugin.NewInstaller(t.TempDir(), "img", "podman"), t.TempDir())
	if _, err := d.UpsertUserWithIdentity("mem", string(permissions.RoleMember), "telegram", "222"); err != nil {
		t.Fatal(err)
	}
	member, _ := d.UserByIdentity("telegram", "222")
	handled, err := r.handleCommand(context.Background(),
		channels.InboundMsg{Channel: "telegram", ChatID: "555", SenderID: "222", Text: "/plugin list"},
		member)
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatal("member /plugin should not be handled (owner-only)")
	}
}

// fakeChannelActivator records Activate/Deactivate calls so the hot-reload hook
// can be tested without a real relay/registry.
type fakeChannelActivator struct {
	activated   []string
	deactivated []string
	failName    string // if set, Activate(failName) returns an error
}

func (f *fakeChannelActivator) Activate(name, pluginDir string) error {
	if name == f.failName {
		return errActivateFail
	}
	f.activated = append(f.activated, name)
	return nil
}

func (f *fakeChannelActivator) Deactivate(name string) {
	f.deactivated = append(f.deactivated, name)
}

var errActivateFail = fmtErr("relay open: boom")

type fmtErr string

func (e fmtErr) Error() string { return string(e) }

// stageManifest writes a minimal plugin.yml of the given kind into <root>/<name>
// and returns the root dir (the pluginDir the router scans).
func stageManifest(t *testing.T, name, kind string) string {
	t.Helper()
	root := t.TempDir()
	pdir := filepath.Join(root, name)
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	var yml string
	switch kind {
	case "channel":
		// A channel manifest must NOT declare a command (manifest validation).
		yml = "name: " + name + "\nkind: channel\nversion: \"1.0.0\"\nexec: " + name + "\ndescription: A channel.\n"
	default:
		yml = "name: " + name + "\nkind: tool\nversion: \"1.0.0\"\nexec: " + name + "\ndescription: A tool.\ncommand: " + name + "\n"
	}
	if err := os.WriteFile(filepath.Join(pdir, "plugin.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestMaybeActivateChannel(t *testing.T) {
	t.Run("channel plugin is activated and confirmed", func(t *testing.T) {
		r, _, _ := cmdSetup(t)
		fa := &fakeChannelActivator{}
		root := stageManifest(t, "irc", "channel")
		r.pluginDir = root
		r.WithChannelActivator(fa)

		note := r.maybeActivateChannel("irc")

		if len(fa.activated) != 1 || fa.activated[0] != "irc" {
			t.Fatalf("expected irc activated, got %v", fa.activated)
		}
		// Re-activation drops the old registration first (idempotent on update).
		if len(fa.deactivated) != 1 || fa.deactivated[0] != "irc" {
			t.Fatalf("expected a Deactivate before Activate, got %v", fa.deactivated)
		}
		if !strings.Contains(note, "now live") {
			t.Fatalf("expected a live-confirmation note, got %q", note)
		}
	})

	t.Run("tool plugin is not activated as a channel", func(t *testing.T) {
		r, _, _ := cmdSetup(t)
		fa := &fakeChannelActivator{}
		root := stageManifest(t, "roll", "tool")
		r.pluginDir = root
		r.WithChannelActivator(fa)

		note := r.maybeActivateChannel("roll")

		if len(fa.activated) != 0 || len(fa.deactivated) != 0 {
			t.Fatalf("a tool plugin must not touch the channel activator: act=%v deact=%v", fa.activated, fa.deactivated)
		}
		if note != "" {
			t.Fatalf("a tool plugin should add no note, got %q", note)
		}
	})

	t.Run("no activator wired is a silent no-op", func(t *testing.T) {
		r, _, _ := cmdSetup(t)
		root := stageManifest(t, "irc", "channel")
		r.pluginDir = root
		// chanAct left nil.
		if note := r.maybeActivateChannel("irc"); note != "" {
			t.Fatalf("nil activator should add no note, got %q", note)
		}
	})

	t.Run("activation failure surfaces in the note and does not panic", func(t *testing.T) {
		r, _, _ := cmdSetup(t)
		fa := &fakeChannelActivator{failName: "irc"}
		root := stageManifest(t, "irc", "channel")
		r.pluginDir = root
		r.WithChannelActivator(fa)

		note := r.maybeActivateChannel("irc")
		if !strings.Contains(note, "failed to activate") {
			t.Fatalf("expected a failure note, got %q", note)
		}
	})
}

// Removing a plugin always tears down any live channel registration for that name,
// even if the dir is already gone (Deactivate is a safe no-op for non-channels).
func TestPluginRemove_DeactivatesChannel(t *testing.T) {
	r, _, _ := cmdSetup(t)
	fa := &fakeChannelActivator{}
	// A real installer whose plugins dir is empty: Remove returns (false, nil).
	r.SetInstaller(plugin.NewInstaller(t.TempDir(), "img", "podman"), t.TempDir())
	r.WithChannelActivator(fa)

	// Even when nothing was installed, remove of a name must not error; and when a
	// plugin IS removed, Deactivate is called. Stage one so Remove reports removed.
	root := stageManifest(t, "irc", "channel")
	r.SetInstaller(plugin.NewInstaller(root, "img", "podman"), root)
	r.WithChannelActivator(fa)

	msg, err := r.pluginRemove("irc")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(msg, "Removed") {
		t.Fatalf("expected removal confirmation, got %q", msg)
	}
	if len(fa.deactivated) != 1 || fa.deactivated[0] != "irc" {
		t.Fatalf("remove should Deactivate the channel, got %v", fa.deactivated)
	}
}
