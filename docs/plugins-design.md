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
is no file watch and no SIGHUP. goclaw adds exactly that: a watched `plugins/`
directory plus a manager that diffs desired-vs-running so plugins can be added,
enabled, disabled, or reloaded live (see "Host side" for the directory-as-registry
model goclaw uses instead of a single manifest file).

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
A plugin's `plugin.yml` declares the env var NAMES it needs (`env: [...]`); the
host supplies the VALUES from its own config at launch (or, later, via the
credential proxy so the plugin process only holds placeholders). Token values are
NEVER written into `plugin.yml` or any on-disk plugin file.

## Host side: the plugin manager

A new `internal/plugin` package in goclaw. Two key shapes:

- A `Client` is the host-side counterpart to the SDK's `Serve`: it launches a
  plugin binary with `exec.CommandContext`, wires the child's stdin/stdout to an
  `ipc.Session` (and its stderr to the host logger), runs the `hello`/`hello.ok`
  handshake, and reads the plugin's `Info`. It exposes `Invoke(tool, args) ->
  (text, isError)` by sending a `FrameRequest` `tool.invoke` and awaiting the
  correlated `FrameResult`. Because the SDK dispatches invokes concurrently and may
  answer out of order, the Client owns an ID generator plus a pending-request map
  (id -> waiting channel); `Recv` runs in one read loop and routes each result to
  its waiter by ID.
- A `Manager` discovers plugins, holds the live `Client`s, supervises them
  (relaunch on unexpected exit, with backoff), and exposes them to the rest of the
  host (the slash-command router and, later, the agent tool bridge).

### Discovery: the directory is the registry

There is NO central hand-edited manifest. The host walks a `plugins/` directory;
each subdirectory is one plugin and ships its own declarative `plugin.yml` (the
author's self-description):

```yaml
# plugins/roll/plugin.yml  (shipped by the plugin author, read-only to the host)
name: roll
kind: tool
version: "1.0.0"
exec: roll                 # binary, relative to this plugin dir
description: Roll dice in NdM notation.
command: roll              # registers the /roll slash command (omit for none)
env: [ ]                   # env var names the plugin needs (host supplies values)
```

Enable/disable is HOST-owned state, kept OUT of the author's `plugin.yml` so the
host never mutates a shipped artifact: a small host-owned sidecar (e.g.
`plugins/.state.yml` mapping plugin name -> enabled, or a `.disabled` marker file
per dir). The `/plugin` chat command edits that sidecar, never `plugin.yml`. The
directory plus the sidecar together ARE the registry; `plugins.yaml` as a single
hand-maintained manifest is dropped.

### Triggers: a plugin tool can fire two ways

The same plugin `tool.invoke` frame is reached by two paths:

- **User slash command** (host-routed, no agent). A plugin that declares
  `command: roll` registers `/roll`. When a user sends `/roll 2d6`, the host's
  message router matches the command, maps the argument string to the tool's args
  (a tool whose schema is a single string field takes the raw remainder; richer
  schemas can define their own parse), invokes the plugin's tool directly, and
  sends the result back to the user. The agent is not involved.
- **Agent tool** (LLM-invoked). The plugin's advertised tools (from the handshake
  `Info.Tools`) are exposed to the in-container agent so the model can call them.
  This path crosses the container boundary (see "Open: agent tool bridge"); it is
  deferred behind the host-side spike below.

Both call the same `Client.Invoke`; only the trigger and where the result goes
differ.

### Hot add and hot reload

An `fsnotify` watch on `plugins/`. On a change the Manager reconciles
desired-vs-running:

- a new plugin dir appears (added by `/plugin add` or dropped in) and is enabled ->
  launch + register its command/tools (HOT ADD, no host restart),
- a plugin disabled via `/plugin disable` (sidecar flips) -> graceful `shutdown`
  control frame, then deregister,
- a plugin's binary or `plugin.yml` changes -> kill and relaunch (a subprocess
  model "reloads code" by replacing the process).

In-flight invokes finish against the process they started on. This needs no host
rebuild (plugins are independent binaries) and no host restart.

### Distribution: all roads end at "a binary in a plugin dir"

The launch path only ever needs a built binary plus a `plugin.yml` in a `plugins/`
subdir. The three ways to get there are all front-ends over that same end state, so
the core is built first and the front-ends are additive:

- baseline: the operator builds (or unpacks) the plugin into `plugins/<name>/`.
- `/plugin add github.com/owner/name`: a convenience that `go build`s the module
  into `plugins/<name>/` and writes nothing the author did not ship (the module
  carries its own `plugin.yml`). Requires the Go toolchain on the host.
- release pull: download a prebuilt binary + `plugin.yml` from a release into
  `plugins/<name>/`. No toolchain on the host.

The `/plugin` command set: `add <module|url>`, `enable <name>`, `disable <name>`,
`list`, `remove <name>`. `enable`/`disable` only flip the host sidecar (instant,
no build); `add`/`remove` change what is on disk.

## Extensibility: channels (and more) later

`Info.Kind` plus the topic-namespaced protocol are the seam. Tools are
`kind: "tool"` using `tool.*` topics. A later `kind: "channel"` reuses the SAME
process model, discovery, manager, supervision, and hot-reload, adding only the
`channel.*` topics (`channel.inbound` as a one-way event, `channel.send` and
`channel.action` as requests) and a host-side shim onto `channels.ChannelAdapter`.
Because features are topics over four fixed frame patterns, adding channels (or a
future `host.*` callback for plugin-to-host config queries) is additive, not a
wire-format change.

## Milestones

goclawkit (the SDK + the `roll` demo) is DONE: `pkg/ipc` (frames/Session),
`pkg/plugin` (Tool/Serve/ServeTool, channel stub), and `cmd/roll`. The host side is
what remains, built spike-first so the container-boundary question is answered with
working code rather than up front.

1. Host-side spike (`internal/plugin`): `Client` (launch `roll`, handshake, the
   invoke + ID-correlation machinery) and a minimal `Manager`. Prove host-to-plugin
   invoke works against the real `roll` binary over OS pipes.
2. Slash-command path: discover `plugins/` dirs + their `plugin.yml`, register
   declared commands, route `/roll 2d6` from the message router to
   `Client.Invoke`, return the result to the user. This is fully host-side (no
   container boundary), so it is the first user-visible plugin feature.
3. The `/plugin` command set + host-owned enable/disable sidecar + `fsnotify`
   reconciliation (hot add/enable/disable/reload).
4. Agent tool bridge: expose plugin tools to the in-container agent (resolved by
   the spike, see open questions), so the LLM can call `roll` too.
5. The `channel` kind: the SDK `ServeChannel` runtime, the `channel.*` topics, the
   host-side `ChannelAdapter` shim, and a reference channel plugin.
6. Layer 2 (only when a plugin needs cross-plugin or plugin-to-host coordination):
   a socket-backed `Transport` plus a host-side hub/registry routing
   topic-addressed frames between plugins, reusing the existing frame format. No
   change to Layer 1 or the wire format.

## Open questions

- Agent tool bridge (the spike resolves this): the agent runs INSIDE the Podman
  container; plugins run on the host. How does a host-launched plugin's tool become
  callable by the in-container agent? Candidates: the host exposes plugin tools as
  an MCP endpoint the container reaches, or proxies tool calls over the existing
  host-mediated channel. The slash-command path needs none of this (it is
  host-only), which is why it ships first.
- Supervision policy: relaunch forever with backoff, or give up after N crashes and
  mark the plugin failed in the sidecar/status?
- Do plugins get the credential proxy from day one, or env tokens first and proxy
  later? Proposed: env first, proxy as a fast follow, since the proxy is already
  built.
