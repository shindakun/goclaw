# Agent-group spec: one declarative definition per group

`internal/agentspec` is the host-side, declarative description of WHAT an agent group
is. Before it, "what is this agent group" was an emergent property of three separate
owners: the base prompt baked into the image, append-prompt string literals hardcoded
in the runner's query loop, and a hardcoded skill map with inline mount-gates in the
runtime composer. The spec collapses those into one value you can read, diff, and
version in a single place.

## The three layers it owns

An `AgentGroupSpec` carries three of the four parts an agent decomposes into:

- **Model**: the model id to run the group with. Empty means the CLI default. Today a
  host-wide default (`GOCLAW_MODEL`); the field is the seam a future per-group
  override sets directly, falling back to the host default.
- **Harness** (`HarnessSpec`): the invariant, model-agnostic core. The baked-in base
  prompt the composed entry point imports, plus always-on `Invariant` blocks appended
  every turn (vault-path discipline, authoritative current time). Invariants are
  TEMPLATES: their text carries `{{placeholders}}` the runner fills with live per-turn
  values, so the wording is host-owned data while no stale value is ever baked in.
  Invariants are tiered (`TierMust` / `TierShould` / `TierMay`) and render in tier
  order; equal-tier blocks keep declared order.
- **Context** (`ContextSpec`): the skills the model auto-invokes by description, each
  with a mount precondition (`RequireNone` / `RequireVault` / `RequireEvents`).
  `ResolvedSkills()` returns the name -> container-target plan for exactly the skills
  whose preconditions the current mounts satisfy. MCP servers are not redefined here;
  they come from installed plugins and are referenced by the runtime.

## What it deliberately does NOT own: I/O

The fourth part of an agent (its I/O: triggers, channels, gateways, delivery
destinations) is **not** in the spec, on purpose. In goclaw the agent is untrusted,
and wiring "which channel/trigger feeds or receives this agent" is a HOST
access-control decision: it lives in the router gate, `agent_destinations`, and the
channel wirings/identities tables, never in a per-group value the agent could
influence. Putting I/O in the spec would hand the untrusted side a say in its own
reachability, which the threat model refuses. Keep I/O host-owned. See
[security.md](security.md).

## How a spec reaches the container

The spec is composed and rendered entirely on the HOST. The container only ever sees
the OUTPUT:

- `internal/runtime` builds the `ContextSpec` from the current mounts and renders it
  into the composed entry-point `CLAUDE.md` and the skill symlinks under the group's
  claude-home (the same artifacts as before; the rendering is just spec-driven now).
- The runner builds the `HarnessSpec`, calls `RenderInvariants` with the live values
  (container mount paths, the current time), and appends each result to the system
  prompt for the turn.
- The Model flows from config into the spec and out to the runner as `GOCLAW_MODEL`,
  which the runner uses when no explicit `-model` flag is set.

The spec is never a live object the container reads or mutates; it renders to files,
symlinks, and env that cross the boundary read-only, exactly like the rest of the
host<->container line. The rendering helpers in `agentspec` are pure and
side-effect-free for that reason: they return a plan, and the host writes it.

## Adding to a group's definition

- A new skill is an append to the `[]SkillRef` in `runtime`'s `groupContextSpec`, with
  its container target and mount requirement. No map edit plus a separate `if`.
- A new always-on instruction is an append to the runner's `harnessInvariants`, with a
  tier and (if it depends on a mount) `RequiresVault`. Use a `{{placeholder}}` for any
  value that varies per turn and fill it at render time; never bake a live value into
  the template.
