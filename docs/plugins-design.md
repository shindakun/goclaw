# goclaw plugins: design

Status: proposal (2026-06). Goal: let operators add a plugin (a tool now, a
channel later) WITHOUT rebuilding the host, and add or reconfigure a plugin WITHOUT
restarting the host. Modeled on the author's own gobbsgo/godoorkit door system and
its `pkg/ipc` protocol.

Ordering note: TOOLS ship first. A tool is request/response (invoke with args,
return a result), which is a clean, small first deliverable; the reference plugin
is a self-contained dice roller. CHANNELS (long-lived bidirectional chat gateways)
are a much larger surface and come later; their contract is designed in now so
they slot onto the same protocol and manager without a wire-format change.

## Why subprocess plugins (not Go `plugin`/.so)

goclaw already runs its agent in a Podman container and talks to it only over two
SQLite files: a process boundary with explicit, narrow communication. A plugin is
the same idea, one notch smaller: a separate process the host launches and talks
to over a defined protocol.

The Go `plugin` package (`buildmode=plugin`, `.so`) is rejected:

- It cannot unload, so there is no true hot-reload of plugin code.
- The plugin must be built with the EXACT host toolchain and identical dependency
  versions or it panics at load. Every host upgrade would force rebuilding every
  plugin.
- Linux/macOS only.

A rolled-own subprocess model (the godoorkit pattern) avoids all three: each
plugin is its own binary, so "reload" is "kill and relaunch", a version mismatch
is a clean handshake rejection rather than a panic, and a crash is contained.

We roll our own protocol (no hashicorp/go-plugin, no gRPC dependency), borrowing
both halves of godoorkit's design: the way it launches binaries and the frame
format from its `pkg/ipc`. The difference from doors is the protocol shape (below).

## The precedent, and the one gap we close

gobbsgo/godoorkit already establish the parts we keep:

- A plugin is a standalone compiled binary the host launches with
  `exec.CommandContext`.
- The host knows only an INTERFACE contract, never the plugin's internals.
- Plugins are listed in a YAML manifest (name, command, executable, enabled,
  metadata) and registered at startup.
- Crash isolation is free: a door cannot take down the BBS.

The gap: gobbsgo loads `doors.yaml` ONCE at boot and registers statically. There
is no file watch and no SIGHUP. goclaw adds exactly that: a watched manifest plus
a manager that diffs desired-vs-running so plugins can be added, removed, or
reconfigured live.

## Two layers (Layer 1 now, Layer 2 eventually)

godoorkit actually ships two distinct mechanisms, and reading its tree settles how
we should borrow them. We take BOTH, at two layers, sharing one frame format.

### Layer 1: host <-> plugin control (BUILD NOW)

How the host launches a plugin and exchanges requests/results with it. The
transport is the plugin's **stdin/stdout pipes**; stderr is reserved for the
plugin's logs, which the host captures into its logger.

This is the direct parallel to a godoorkit door, with one honest adaptation: a
door's stdio carries its TERMINAL session (raw keystrokes in, ANSI screen out), and
its control needs are met by a drop file plus the process exiting. A goclaw plugin
has NO terminal, so its stdio is free to carry the control protocol itself
(handshake, invoke, result). Every plugin needs Layer 1; it is the whole of the
first deliverable. Lifecycle is dead simple: the process dies, the connection is
gone; "reload" is kill-and-relaunch.

### Layer 2: plugin <-> plugin / plugin <-> host coordination (EVENTUALLY)

A shared bus for cross-plugin state, presence, or broadcast: the exact role of
godoorkit's `pkg/ipc`, a Unix-socket hub daemon that clients dial. It is a
ROUTER plugins connect to, not the host-to-plugin control path.

We do not build Layer 2 now, because nothing in the v1 plugin set (tools, then
channels) needs plugins to talk to EACH OTHER. A tool answers an invoke; a channel
streams its own messages; neither coordinates with a sibling plugin. Layer 2 earns
its place only when a plugin genuinely needs cross-plugin coordination.

When Layer 2 is warranted (the first plugin that needs shared cross-plugin state,
a presence feed, or a broadcast, or a `host.*` callback so a plugin can query host
config at runtime), it lands as:

- a `Transport` implementation backed by a Unix socket (`Dial`/`Listen`) instead of
  pipes, carrying the SAME frames, and
- a small host-side hub/registry that accepts plugin connections, tracks them, and
  routes `topic`-addressed frames between them (mirroring `pkg/ipc`'s hub,
  registry, heartbeats, and stale detection).

Because the frame format is shared and `Transport` is an interface from day one,
Layer 2 is purely additive: a new transport plus a router, with NO change to the
wire format and NO change to Layer 1. A plugin opts into the bus the way a
godoorkit door would (it dials the hub); a plugin that ignores it is unaffected,
exactly as a single-node door ignores `pkg/ipc` today.

## Two plugin shapes: tools (first) and channels (later)

A TOOL is request/response: the host sends an invoke with args, the plugin runs and
returns a result. This maps directly onto the framed request/result pattern and is
the first thing we build.

A CHANNEL is different: it streams inbound messages continuously AND accepts
outbound sends concurrently, for the life of the host. It needs the same framed
protocol plus the one-way event pattern (inbound pushes) running alongside
requests. The protocol below is designed so both shapes ride the same frames; the
channel work is deferred, not redesigned-around.

Both shapes require a protocol that is:

- FRAMED (discrete messages, not a raw byte stream), and
- MULTIPLEXED (results and one-way events can interleave, correlated by id).

## The contract (goclawkit)

A new module `github.com/shindakun/goclawkit` holds the plugin-author SDK: the
types, the framed protocol, and a `Serve` helper so a plugin author writes only the
interesting part. goclaw imports the same module host-side for the types. This
mirrors how godoorkit is the kit and gobbsgo is the host. The full SDK spec lives
in `goclawkit/IMPLEMENTATION.md`; that file is the source of truth for the wire
format. This section summarizes only what the host side needs.

### Plugin-author surface (tools first)

A tool plugin implements a minimal interface and calls `Serve`:

```go
// in goclawkit
type Tool interface {
    // Info returns the tool's name, description, and input JSON Schema.
    Info() ToolInfo
    // Invoke runs the tool with the agent-supplied args and returns result text
    // (or an error, which the host surfaces to the agent as an error result).
    Invoke(ctx context.Context, args json.RawMessage) (string, error)
}

func main() {
    goclawkit.ServeTool(rollTool{}, "roll", "1.0.0") // one line
}
```

The channel interface (`Start`/`Send`, mirroring goclaw's `channels.ChannelAdapter`
so plugin channels are indistinguishable to the router) is defined in the SDK as a
forward-looking stub and wired in the later channel milestone. See the SDK spec.

### The wire protocol (rolled own, length-prefixed frames over stdio)

The full definition is in the SDK spec; the design decisions that matter host-side:

- Transport: the plugin's **stdin and stdout** (stderr is reserved for the
  plugin's logs, which the host fans into its own logger, exactly as gobbsgo does
  with door stderr). Transport is an interface (stdio now) so a networked plugin
  later is not a rewrite.
- Framing: **length-prefixed binary frames** (a fixed header: magic `GCLW`,
  version, frame type, id, topic, then a length-prefixed opaque JSON payload), the
  same approach as godoorkit's `pkg/ipc`. Length-prefixing means arbitrary payload
  bytes with an explicit cap and no line-length traps.
- Extensibility by TOPIC, not by frame type. There are only four frame patterns
  (`control`, `request`, `result`, `event`); every feature is a dot-namespaced
  `topic` plus an opaque payload. The handshake is `control`/`hello`, a tool call
  is `request`/`tool.invoke` answered by a correlated `result`, shutdown is
  `control`/`shutdown`. Channels later add `event`/`channel.inbound`,
  `request`/`channel.send`, `request`/`channel.action`: NEW TOPICS, no new frame
  type and no version bump. An unknown topic is answered with an error result (for
  a request) or ignored (for an event), never a crash; that tolerance is what lets
  a newer peer talk to an older one. The wire format is meant to freeze early.

Credentials: like every other goclaw secret, a plugin's tokens stay on the host.
The host passes them in the plugin's environment (or, later, via the credential
proxy so the plugin process only holds placeholders). Tokens are NEVER written
into the manifest.

## Host side: the plugin manager

A new `internal/plugin` package in goclaw:

- Reads `plugins.yaml` (the manifest): a list of `{name, exec, enabled, env_keys,
  ...}` entries, parallel to gobbsgo's `doors.yaml`.
- For each enabled entry, launches the binary with `exec.CommandContext`, performs
  the handshake (reading the plugin's `Info`, including its `Kind`), and wraps the
  live process in a host-side adapter that translates method calls to/from the
  frame protocol. Dispatch is by `Kind`: a `tool` plugin's advertised tools are
  registered so the agent can call them (each call becomes a `tool.invoke`
  request); a `channel` plugin (later) is wrapped to satisfy the existing
  `channels.ChannelAdapter` interface and registered in the channel registry, so
  routing/delivery treat it like any built-in channel.
- Supervises: if a plugin process exits unexpectedly, the manager logs it and (per
  policy) relaunches with backoff. A crashing plugin cannot take down the host.

### Hot add and hot reload (the two requirements)

- Config hot-reload: an `fsnotify` watch on `plugins.yaml`. On change, the manager
  re-reads the manifest and diffs desired-vs-running:
  - entry newly `enabled` -> launch + register (HOT ADD, no host restart),
  - entry removed or `enabled: false` -> graceful `shutdown` then deregister,
  - entry's exec/env changed -> kill and relaunch (this is how a subprocess model
    does "reload code": replace the process).
  The manifest pointer is swapped atomically; in-flight sends finish against the
  process they started on.
- This needs no host rebuild (plugins are independent binaries) and no host
  restart (the manager reconciles live).

## Extensibility: channels (and more) later

`Info.Kind` plus the topic-namespaced protocol are the seam. Tools are
`kind: "tool"` using `tool.*` topics. A later `kind: "channel"` reuses the SAME
process model, manifest, manager, supervision, and hot-reload, adding only the
`channel.*` topics (`channel.inbound` as a one-way event, `channel.send` and
`channel.action` as requests) and a host-side shim onto `channels.ChannelAdapter`.
Because features are topics over four fixed frame patterns, adding channels (or a
future `host.*` callback for plugin-to-host config queries) is additive, not a
wire-format change.

## Milestones

Layer 1 throughout. Layer 2 is the final, conditional milestone.

1. goclawkit: the framed protocol (proto.go) + `Serve` + handshake + the `Tool`
   contract, plus the worked `roll` demo. (Spec: `goclawkit/IMPLEMENTATION.md`.)
2. goclaw `internal/plugin`: manager, manifest load, launch, handshake, and the
   tool-registration path so the agent can call a plugin tool. Static (no watch
   yet).
3. The fsnotify watch + desired-vs-running reconciliation (hot add / reload).
4. The `channel` kind: the SDK `ServeChannel` runtime, the `channel.*` topics, the
   host-side `ChannelAdapter` shim, and a reference channel plugin.
5. Layer 2 (only when a plugin needs cross-plugin or plugin-to-host coordination):
   a socket-backed `Transport` plus a host-side hub/registry routing
   topic-addressed frames between plugins, reusing the existing frame format. No
   change to Layer 1 or the wire format.

## Open questions

- Supervision policy: relaunch forever with backoff, or give up after N crashes
  and mark the plugin failed in the manifest/status?
- Manifest vs directory scan: explicit `plugins.yaml` entries (gobbsgo's model,
  predictable) vs scanning a `plugins/` dir for binaries (more "marketplace", less
  explicit). Proposed: manifest now, optional scan later.
- Do plugins get the credential proxy from day one, or env tokens first and proxy
  later? Proposed: env first, proxy as a fast follow, since the proxy is already
  built.
