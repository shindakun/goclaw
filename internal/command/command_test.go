package command

import (
	"context"
	"strings"
	"testing"

	"github.com/shindakun/goclaw/internal/permissions"
)

func TestIsCommand(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantArgs string
		wantOk   bool
	}{
		{"/roll 2d6", "roll", "2d6", true},
		{"/roll", "roll", "", true},
		{"  /Roll   3d8  ", "roll", "3d8", true}, // trimmed + lower-cased
		{"/commands", "commands", "", true},
		{"hello", "", "", false},
		{"/", "", "", false},
		{"", "", "", false},
		{"not /a command", "", "", false}, // slash must be at the start
	}
	for _, c := range cases {
		name, args, ok := IsCommand(c.in)
		if name != c.wantName || args != c.wantArgs || ok != c.wantOk {
			t.Errorf("IsCommand(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, name, args, ok, c.wantName, c.wantArgs, c.wantOk)
		}
	}
}

func TestRegistry_RegisterGetUnregister(t *testing.T) {
	r := NewRegistry()
	r.Register(Command{Name: "/Roll", Source: "roll", Handler: noop})

	c, ok := r.Get("roll") // slash optional, case-insensitive
	if !ok || c.Name != "roll" {
		t.Fatalf("Get returned %+v, %v", c, ok)
	}
	if c.MinRole != permissions.RoleMember {
		t.Fatalf("empty MinRole should default to member, got %q", c.MinRole)
	}

	r.Unregister("/roll")
	if _, ok := r.Get("roll"); ok {
		t.Fatal("expected roll gone after Unregister")
	}
}

func TestRegistry_UnregisterSource(t *testing.T) {
	r := NewRegistry()
	r.Register(Command{Name: "roll", Source: "roll", Handler: noop})
	r.Register(Command{Name: "weather", Source: "weather", Handler: noop})
	r.Register(Command{Name: "commands", Source: "builtin", Handler: noop})

	r.UnregisterSource("roll")
	if _, ok := r.Get("roll"); ok {
		t.Fatal("roll should be gone")
	}
	if _, ok := r.Get("weather"); !ok {
		t.Fatal("weather should remain")
	}
	if _, ok := r.Get("commands"); !ok {
		t.Fatal("builtin should remain")
	}
}

func TestRegistry_VisibleByRole(t *testing.T) {
	r := NewRegistry()
	r.Register(Command{Name: "roll", MinRole: permissions.RoleMember, Handler: noop})
	r.Register(Command{Name: "approve", MinRole: permissions.RoleOwner, Handler: noop})

	member := names(r.Visible(permissions.RoleMember))
	if has(member, "approve") {
		t.Fatalf("member must not see owner-only approve: %v", member)
	}
	if !has(member, "roll") {
		t.Fatalf("member should see roll: %v", member)
	}

	owner := names(r.Visible(permissions.RoleOwner))
	if !has(owner, "approve") || !has(owner, "roll") {
		t.Fatalf("owner should see both: %v", owner)
	}

	// An unknown/empty role sees nothing role-gated at member or above.
	if got := names(r.Visible("")); len(got) != 0 {
		t.Fatalf("empty role should see nothing, got %v", got)
	}
}

// /commands listing renders visible commands, groups plugins under their name, and
// orders builtin first.
func TestListingRender(t *testing.T) {
	r := NewRegistry()
	RegisterListing(r) // adds /commands, /help, /reset, /compact
	r.Register(Command{Name: "roll", Description: "Roll dice.", Source: "roll", Handler: noop})

	out := r.render(permissions.RoleMember)
	if !strings.Contains(out, "/commands") || !strings.Contains(out, "/reset") {
		t.Fatalf("listing missing builtins:\n%s", out)
	}
	if !strings.Contains(out, "/roll - Roll dice.") {
		t.Fatalf("listing missing plugin command:\n%s", out)
	}
	// The plugin source gets its own header; builtin commands come first.
	if !strings.Contains(out, "\nroll:\n") {
		t.Fatalf("expected a 'roll:' group header:\n%s", out)
	}
	if strings.Index(out, "/commands") > strings.Index(out, "/roll") {
		t.Fatalf("builtin commands should list before plugin commands:\n%s", out)
	}
}

func TestPassThroughHasNoHandler(t *testing.T) {
	r := NewRegistry()
	RegisterListing(r)
	reset, ok := r.Get("reset")
	if !ok || !reset.PassThrough || reset.Handler != nil {
		t.Fatalf("reset should be a pass-through with no handler, got %+v ok=%v", reset, ok)
	}
}

func noop(_ context.Context, _ Request) (string, error) { return "", nil }

func names(cmds []Command) []string {
	out := make([]string, len(cmds))
	for i, c := range cmds {
		out[i] = c.Name
	}
	return out
}

func has(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
