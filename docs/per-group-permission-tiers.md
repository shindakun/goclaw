# Per-agent-group permission tiers

Status: design (not built). This proposes expressing a small set of named
permission **tiers** per agent group, where a tier is a bundle of CONTAINER FACTS
(network mode, mount writability) applied once at `podman run`, not a runtime gate
that re-decides each tool call.

## Motivation

Today every runner container is launched with one uniform posture
(`internal/runtime.buildArgs`): non-root (`--user 1000:1000`), `--init`, its
validated mounts, its injected env, and podman's default network. That single
posture is a strong floor, but it is the SAME floor for every agent group. There is
no way to say "this group is read-only" or "this group has no network", even though
those are exactly the distinctions that bound blast radius when you wire up a group
whose job does not need write access or outbound connectivity.

A permission tier makes that distinction first-class and declarative: an operator
labels a group, and the label resolves to a different (tighter) container launch.
Because the difference is a launch fact, the boundary is structural and unforgeable
from inside the container, consistent with goclaw's design stance that containment
is enforced by the container, not by inspecting the agent's behavior.

## Non-goal: command-level gating

This explicitly does NOT add a per-command allow/deny gate (a "safe commands" list
the agent's `bash` calls are checked against). That kind of gate is a pattern-match
on agent behavior, which is bypassable and gives false confidence; it is the wrong
layer for goclaw. Inside a tight container (no network, no sensitive mounts,
non-root), a destructive command like `rm -rf` damages only the agent's own
workspace, which the container already bounds. Re-litigating that per command buys
nothing and adds a surface to evade. The tier model deliberately stays at the level
of container capability, never command inspection.

## Tiers

The tiers are points on a "loosen as trust grows" scale. Names are illustrative;
the load-bearing part is the bundle of facts each one sets.

| Tier        | Network              | Mount writability                      | Intended use                                  |
|-------------|----------------------|----------------------------------------|-----------------------------------------------|
| `observer`  | none                 | all mounts forced read-only            | a new/untrusted group; look but do not touch  |
| `worker`    | restricted/proxy-only| writable only where the allowlist says | day-to-day; bounded outbound                   |
| `standard`  | default              | per allowlist                          | the current behavior; the default tier        |
| `trusted`   | default              | per allowlist                          | same as standard, named for intent/audit      |

Two facts carry almost all the value:

### Network mode (highest value)

`buildArgs` currently emits no `--network` flag, so containers get podman's default
network. A tier can set:

- **`--network=none`** for an `observer` group: no outbound connectivity at all.
  This is the single most valuable tier setting because the network is the
  exfiltration and command-and-control path, and `--network=none` is one
  unforgeable flag that removes it entirely. A group that only summarizes mounted
  files, or one being evaluated before it is trusted, does not need the network.
- **proxy-only / restricted** for a `worker` group: reachable hosts limited to what
  the credential proxy already mediates, so outbound traffic is funneled rather than
  open. (Exact mechanism: a dedicated podman network, or relying on the existing
  proxy plus dropping default egress; to be settled at build time.)

### Mount writability (tighten-only)

`mounts.Validate` already supports asymmetric read-only / read-write per allowlist
entry. A tier does not need new mount logic; it only needs to CAP what is requested:

- An `observer` tier forces every resolved mount to `ReadWrite=false` regardless of
  what the allowlist entry would permit. This is tighten-only: a tier may make a
  mount read-only, it may NEVER widen a read-only entry to read-write. The allowlist
  remains the ceiling; the tier can only lower from there. That ordering matters so a
  tier can never become a privilege-escalation path around the allowlist.

A `standard`/`trusted` tier changes nothing about mounts; it is the current behavior,
named so an operator and the audit trail can see the intent.

## Shape (sketch, not final)

A `Tier` value on the agent-group config resolves to a small descriptor the runtime
consults while assembling the launch:

```go
// Tier is the resolved bundle of container facts for a group's posture. It is
// applied at launch; nothing about it is re-evaluated per tool call.
type Tier struct {
    Network       string // "" = podman default; "none"; "proxy-only"
    ForceReadOnly bool   // cap every mount to read-only, never widen
}
```

`buildArgs` gains, conceptually:

```go
if spec.Tier.Network != "" {
    args = append(args, "--network", spec.Tier.Network)
}
```

and the mount-validation step applies `ForceReadOnly` by clearing `ReadWrite` on
each request before (or on the result of) `Validate`, so the cap composes with the
allowlist ceiling rather than bypassing it.

Resolution is fail-safe: an unknown or unset tier resolves to the current
`standard` posture (the existing default), never to something looser than today.

## Interactions and constraints

- **Credential proxy.** A `--network=none` tier cannot reach the proxy, so it is
  incompatible with proxy-mediated credentials (and with any task needing the
  Anthropic API). That is correct for an `observer` group (it should not be making
  model calls or reaching anything), but it means tier and credential mode are not
  independent: choosing `observer` implies "no outbound, including the model". The
  config surface should surface that, not let an operator pick a contradictory pair
  silently.
- **Channel-hosting groups.** A group that hosts an always-on channel plugin needs
  outbound connectivity to stay connected, so it cannot be `observer`. Tier
  assignment must refuse (or warn loudly about) a network-less tier on a group that
  hosts a channel.
- **One posture per group, set at launch.** A tier is chosen when the group is
  configured and applied when its container launches. Changing a group's tier means
  recreating its container (same as changing its mounts today). There is no runtime
  tier-switch; that would imply a live control channel into the container, which the
  boundary refuses.
- **Audit.** Tier belongs in the launch event (`runner.launched`) so the
  operational log records the posture each container ran with.

## What needs deciding before building

These are product/UX calls, not mechanism questions:

1. **How an operator assigns a tier** to a group (config field name, CLI surface,
   default for newly wired groups). The default must be the current `standard`
   posture so existing deployments are unchanged.
2. **The exact "restricted/proxy-only" network mechanism** for `worker`: dedicated
   podman network vs. proxy-plus-egress-drop, and how it coexists with the existing
   `http_proxy`/`NO_PROXY` env the proxy path already sets.
3. **Whether `worker`/`trusted` earn their keep** or whether the meaningful set is
   just `observer` (locked down) and `standard` (today). Fewer tiers with crisp,
   load-bearing differences beats a granular ladder whose middle rungs differ only
   cosmetically.

## Home for the tier: the agent-group spec

When this lands, the tier belongs on the host-side `agentspec.AgentGroupSpec` (see
[agent-group-spec.md](agent-group-spec.md)), alongside Model/Harness/Context, as a
`Tier` (or `Capabilities`) field the runtime reads while assembling the launch. That
keeps it consistent with the rest of the per-group definition: host-owned, rendered
into container facts, never I/O. The `Spec.Tier` sketch above predates that package;
read it as "a field on the agent-group spec", not a separate struct.

## Sequencing relative to capability hardening

The single existing posture is a strong floor in most respects (non-root, rootless,
explicit mounts, no host networking, no socket), so no group is currently
UNDER-contained relative to that floor. But the floor itself is not yet at the
minimum-capability mark: the container still launches with podman's DEFAULT
capability set, no `--cap-drop=ALL`, no `--security-opt=no-new-privileges`, no
seccomp profile, no read-only rootfs (see the residual-risk note in
[security.md](security.md)).

That capability-hardening work and this tier work touch the SAME surface
(`buildArgs` / the launch argv) and should be sequenced together: harden the
always-on floor first (drop-all-caps + no-new-privileges as the new default for every
group), then expose the per-group tightening (network mode, mount writability, any
caps a group may add back) as tier fields on the agent-group spec. Doing tiers first
would mean revisiting the same argv twice. So this is still an enhancement rather than
a present under-containment, but it is the natural follow-on to the capability floor,
not an indefinitely deferred item.
