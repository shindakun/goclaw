package agentspec

import (
	"reflect"
	"testing"
)

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
