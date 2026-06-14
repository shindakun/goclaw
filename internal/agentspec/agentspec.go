// Package agentspec is the host-side, declarative description of WHAT an agent
// group is: its model, its harness (the invariant, model-agnostic core: base prompt
// + always-on instruction tiers), and its context (the skills layered on top). It is
// pure data plus pure rendering: it computes the PLAN (which skills link to which
// container targets, what harness invariant text to inject), and the runtime package
// executes that plan into files, symlinks, and CLI options.
//
// Why this exists: "what is an agent group" was previously an emergent property of
// three separate owners (the base prompt baked in the image, append-prompt string
// literals hardcoded in the runner's query loop, and a hardcoded skill map with
// inline mount-gates in the runtime composer). Collapsing those into one value makes
// a group's definition readable, diffable, and versionable in a single place.
//
// Containment boundary (do not erode):
//
//   - This describes only Model / Harness / Context. It deliberately does NOT carry
//     I/O (channels, triggers, gateways, delivery destinations). In goclaw the agent
//     is untrusted and I/O wiring is a HOST access-control decision (the router gate,
//     agent_destinations, channel wirings); it must never be something a per-group
//     agent spec can influence. Keep I/O out of this package.
//   - A spec is composed on the HOST and crosses to the container only as rendered
//     artifacts (a composed prompt file, skill symlinks, env, CLI options) that the
//     container reads. The spec is never a live object the container mutates. The
//     rendering helpers here are pure and side-effect-free for exactly that reason:
//     they return a plan, and the host writes it.
package agentspec

import "strings"

// AgentGroupSpec is the complete host-side definition of one agent group's agent.
// The zero value is a usable minimal spec (host-default model, base harness, no
// extra context); callers build it up from group config and current mounts.
type AgentGroupSpec struct {
	// Model is the model identifier to run this group with (e.g. an Anthropic model
	// id). Empty means "use the host default", so existing groups that declare no
	// model are unchanged.
	Model string

	Harness HarnessSpec
	Context ContextSpec
}

// HarnessSpec is the invariant, model-agnostic core: the base system prompt plus the
// always-on instruction tiers the host injects on every turn. It is the part that
// does not vary by task, only by the agent group's fixed role and its mounts.
type HarnessSpec struct {
	// BasePromptContainerPath is the container path of the baked-in base prompt the
	// composed entry point imports (today: /app/CLAUDE.md). It is a CONTAINER path:
	// it resolves inside the container, not on the host.
	BasePromptContainerPath string

	// Invariants are always-on instruction blocks appended to the system prompt every
	// turn, in order. They are templates: a block whose text depends on per-turn data
	// (e.g. the current time) carries a placeholder the runner fills at query time;
	// the host never bakes a stale value in. See Invariant.
	Invariants []Invariant
}

// InvariantTier ranks an always-on instruction by how binding it is, mirroring the
// must/should/may tiering of the agent prompt. The tier is metadata for ordering and
// future policy; it does not change that every listed invariant is injected.
type InvariantTier int

const (
	// TierMust is a hard rule the agent may not violate (e.g. authoritative time,
	// vault-path discipline). These render first.
	TierMust InvariantTier = iota
	// TierShould is a strong default the agent follows unless a task overrides it.
	TierShould
	// TierMay is a soft hint.
	TierMay
)

// Invariant is one always-on instruction block. Text may contain named placeholders
// of the form {{name}} that the runner substitutes at query time from live values
// (the only supported placeholder today is {{now}}); a block with no placeholder is
// emitted verbatim. Keeping the TEXT here (data) and the VALUE at the runner (live)
// is what lets the host own the wording without baking a stale per-turn value.
type Invariant struct {
	Name string        // stable identifier, e.g. "authoritative-time", "vault-path"
	Tier InvariantTier // ordering/severity metadata
	Text string        // the instruction text, possibly with {{placeholder}} tokens
	// RequiresVault gates this invariant on a mounted vault: a vault-path instruction
	// is meaningless (and misleading) without one, so it is only emitted when true and
	// a vault is present. A false value means "always emit".
	RequiresVault bool
}

// ContextSpec is the per-group layered context: the skills the model auto-invokes by
// description. MCP servers are NOT redefined here; they come from installed plugins
// and are referenced by the runtime, not owned by the spec.
type ContextSpec struct {
	// Skills is the ordered set of skills to link into the group's claude-home. Each
	// SkillRef knows its container target and any mount precondition; the runtime
	// renders exactly the skills whose preconditions the current mounts satisfy.
	Skills []SkillRef

	// VaultMounted / EventsMounted reflect the group's current mounts. They gate
	// vault- and events-dependent skills and harness invariants. They are inputs to
	// rendering, not policy: the host sets them from what it actually mounted.
	VaultMounted  bool
	EventsMounted bool
}

// SkillRef is one skill the agent can auto-invoke. Name is the link name under
// skills/; ContainerTarget is the symlink target (a container path that dangles on
// the host and resolves in the container). Requires gates the skill on a mount.
type SkillRef struct {
	Name            string
	ContainerTarget string
	Requires        MountRequirement
}

// MountRequirement is the mount precondition for a skill or invariant.
type MountRequirement int

const (
	// RequireNone: always present.
	RequireNone MountRequirement = iota
	// RequireVault: present only when a vault is mounted.
	RequireVault
	// RequireEvents: present only when the event log is mounted.
	RequireEvents
)

// RenderInvariants returns the harness invariant texts to append to the system
// prompt this turn, in tier order (TierMust first), with every {{name}} placeholder
// substituted from values. An invariant gated RequiresVault is skipped unless
// vaultMounted is true. Unknown placeholders are left untouched (so a typo is
// visible rather than silently dropped). Pure: it reads data and returns strings.
//
// Keeping the template TEXT in the spec and the VALUES at the call site is the whole
// point: the host owns the wording, the runner supplies live per-turn data (the
// current time, the container mount paths), and no stale value is ever baked in.
func (h HarnessSpec) RenderInvariants(vaultMounted bool, values map[string]string) []string {
	// Stable tier order without sorting the original slice.
	ordered := make([]Invariant, len(h.Invariants))
	copy(ordered, h.Invariants)
	// Simple insertion sort by Tier keeps equal-tier invariants in declared order.
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && ordered[j-1].Tier > ordered[j].Tier; j-- {
			ordered[j-1], ordered[j] = ordered[j], ordered[j-1]
		}
	}
	var out []string
	for _, inv := range ordered {
		if inv.RequiresVault && !vaultMounted {
			continue
		}
		out = append(out, substitute(inv.Text, values))
	}
	return out
}

// substitute replaces each {{name}} in text with values[name]; an unknown name is
// left as-is so a mistake surfaces in the prompt rather than vanishing.
func substitute(text string, values map[string]string) string {
	for name, val := range values {
		text = strings.ReplaceAll(text, "{{"+name+"}}", val)
	}
	return text
}

// ResolvedSkills returns the skills whose mount precondition the spec's current
// mounts satisfy, as a name -> container-target map ready for the runtime to sync as
// symlinks. Pure: it reads the spec and returns a plan, writing nothing.
func (s ContextSpec) ResolvedSkills() map[string]string {
	out := make(map[string]string, len(s.Skills))
	for _, sk := range s.Skills {
		switch sk.Requires {
		case RequireVault:
			if !s.VaultMounted {
				continue
			}
		case RequireEvents:
			if !s.EventsMounted {
				continue
			}
		}
		out[sk.Name] = sk.ContainerTarget
	}
	return out
}
