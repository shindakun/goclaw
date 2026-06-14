package agentspec

import (
	"reflect"
	"strings"
	"testing"
)

func TestRenderInvariants_TierOrderAndGating(t *testing.T) {
	h := HarnessSpec{Invariants: []Invariant{
		{Name: "may-hint", Tier: TierMay, Text: "soft hint"},
		{Name: "vault-path", Tier: TierMust, Text: "vault at {{vault_dir}}", RequiresVault: true},
		{Name: "time", Tier: TierMust, Text: "now is {{now}}"},
		{Name: "should-default", Tier: TierShould, Text: "a default"},
	}}
	vals := map[string]string{"now": "2026-06-14 10:00 UTC", "vault_dir": "/vault"}

	// Vault mounted: all four, in tier order (Must, Must, Should, May). Equal-tier
	// invariants keep declared order, so vault-path (declared before time) precedes it.
	got := h.RenderInvariants(true, vals)
	want := []string{
		"vault at /vault",
		"now is 2026-06-14 10:00 UTC",
		"a default",
		"soft hint",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("vault-mounted render =\n%#v\nwant\n%#v", got, want)
	}

	// No vault: the RequiresVault invariant is dropped; the rest keep order.
	gotNo := h.RenderInvariants(false, vals)
	wantNo := []string{
		"now is 2026-06-14 10:00 UTC",
		"a default",
		"soft hint",
	}
	if !reflect.DeepEqual(gotNo, wantNo) {
		t.Fatalf("no-vault render =\n%#v\nwant\n%#v", gotNo, wantNo)
	}
}

func TestRenderInvariants_UnknownPlaceholderLeftIntact(t *testing.T) {
	h := HarnessSpec{Invariants: []Invariant{{Name: "x", Text: "value is {{missing}} here"}}}
	got := h.RenderInvariants(false, map[string]string{"now": "irrelevant"})
	if len(got) != 1 || !strings.Contains(got[0], "{{missing}}") {
		t.Fatalf("unknown placeholder should survive, got %#v", got)
	}
}

func TestRenderInvariants_DoesNotMutateSpecOrder(t *testing.T) {
	h := HarnessSpec{Invariants: []Invariant{
		{Name: "b", Tier: TierShould, Text: "b"},
		{Name: "a", Tier: TierMust, Text: "a"},
	}}
	_ = h.RenderInvariants(false, nil)
	// The original slice order is unchanged (render sorts a copy).
	if h.Invariants[0].Name != "b" || h.Invariants[1].Name != "a" {
		t.Fatalf("RenderInvariants mutated the spec's invariant order: %v", h.Invariants)
	}
}

func baseSkills() []SkillRef {
	return []SkillRef{
		{Name: "coding", ContainerTarget: "/app/skills/coding", Requires: RequireNone},
		{Name: "librarian", ContainerTarget: "/vault/.claude/skills/librarian", Requires: RequireVault},
		{Name: "introspection", ContainerTarget: "/app/skills/introspection", Requires: RequireEvents},
	}
}

func TestResolvedSkills_GatesByMounts(t *testing.T) {
	cases := []struct {
		name          string
		vault, events bool
		want          map[string]string
	}{
		{"none", false, false, map[string]string{
			"coding": "/app/skills/coding",
		}},
		{"vault", true, false, map[string]string{
			"coding":    "/app/skills/coding",
			"librarian": "/vault/.claude/skills/librarian",
		}},
		{"events", false, true, map[string]string{
			"coding":        "/app/skills/coding",
			"introspection": "/app/skills/introspection",
		}},
		{"both", true, true, map[string]string{
			"coding":        "/app/skills/coding",
			"librarian":     "/vault/.claude/skills/librarian",
			"introspection": "/app/skills/introspection",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ContextSpec{Skills: baseSkills(), VaultMounted: tc.vault, EventsMounted: tc.events}
			got := c.ResolvedSkills()
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ResolvedSkills() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolvedSkills_EmptyWhenNoSkills(t *testing.T) {
	c := ContextSpec{VaultMounted: true, EventsMounted: true}
	if got := c.ResolvedSkills(); len(got) != 0 {
		t.Fatalf("expected empty map for no skills, got %v", got)
	}
}

// A RequireNone skill is present regardless of mounts; this guards against the gate
// accidentally hiding an always-on skill.
func TestResolvedSkills_RequireNoneAlwaysPresent(t *testing.T) {
	c := ContextSpec{
		Skills:        []SkillRef{{Name: "coding", ContainerTarget: "/app/skills/coding", Requires: RequireNone}},
		VaultMounted:  false,
		EventsMounted: false,
	}
	got := c.ResolvedSkills()
	if got["coding"] != "/app/skills/coding" {
		t.Fatalf("RequireNone skill missing with no mounts: %v", got)
	}
}
