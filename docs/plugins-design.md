# goclaw plugins: design

Status: SHIPPED. Operators add a plugin (`/plugin add <git-url>`) WITHOUT rebuilding
or restarting the host. Both kinds are built: TOOL plugins (request/response MCP
servers) and CHANNEL plugins (long-lived chat gateways, see
`docs/channels-plugin-design.md`), on the same protocol and manager (`internal/plugin`,
the in-container launch in `cmd/claude-runner/plugins.go`). This is the foundational
design doc, retained as the record; the sections below describe what was built.
Modeled on the author's own gobbsgo/godoorkit door system and its `pkg/ipc` protocol.

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
interesting part. This mirrors how godoorkit is the kit and gobbsgo is the host.

The goclaw host does NOT import goclawkit. A plugin is a black-box compiled binary;
the host speaks the wire protocol TO it, so the host carries its own small copy of
the protocol constants and payload structs (a frozen wire contract, not shared Go
code). Coupling the host to the SDK module would mean the host rebuilds when the
SDK changes, which defeats the no-rebuild goal. The full SDK spec lives in
`goclawkit/docs/sdk-spec.md` and is the source of truth for the wire format both
sides implement; this section summarizes only what the host side needs.

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

There is NO central hand-edited manifest. The host walks the plugins directory
(`<data_dir>/plugins/`, i.e. `data/plugins/` by default; it is installed runtime
state, so it lives under the data dir, not the repo root). Each subdirectory is one
plugin and ships its own declarative `plugin.yml` (the author's self-description).
Below, `plugins/<name>/` is shorthand for `<data_dir>/plugins/<name>/`:

```yaml
# plugins/roll/plugin.yml  (shipped by the plugin author, read-only to the host)
name: roll
kind: tool
version: "1.0.0"
author: shindakun
git: https://github.com/shindakun/roll   # source/home; `/plugin add` builds from here
exec: roll                 # binary, relative to this plugin dir
description: Roll dice in NdM notation.
command: roll              # registers the /roll slash command (omit for none)
env: [ ]                   # env var names the plugin needs (host supplies values)
```

`author` and `git` are provenance: `author` shows in `/plugin list`, and `git` is
the source URL `/plugin add <url>` builds or pulls from (and what `/plugin update`
would re-fetch). Both are informational to the host launch path; only `git` is
load-bearing, and only for the add/update front-ends, not for launching an
already-present plugin.

The directory IS the registry: a plugin is installed iff its dir is present, so
`/plugin add` and `/plugin remove` are just "create the dir" and "delete the dir".
There is no central `plugins.yaml` manifest. (An enable/disable-WITHOUT-removing
toggle, which would need a host-owned sidecar so the author's `plugin.yml` is never
mutated, is an optional follow-up; today removal is the off switch.)

### Triggers: a plugin tool can fire two ways

The same plugin `tool.invoke` frame is reached by two paths, and since plugins run
IN the agent container (see "Where plugins run"), both resolve there:

- **User slash command** (no LLM turn). A plugin that declares `command: roll`
  registers `/roll`. The host routes a plugin slash command inward (it is a
  pass-through in the host's `/commands` registry); the in-container runner
  intercepts `/roll 2d6`, maps the argument string to the tool's args (a single
  string-field schema takes the raw remainder), invokes the plugin's tool directly,
  and the reply returns to the user. The agent is not involved.
- **Agent tool** (LLM-invoked). The plugin's advertised tools (from the handshake
  `Info.Tools`) are registered as local MCP tools the agent can call in-process to
  the container, no boundary crossing.

Both call the same plugin `Client.Invoke` inside the container; only the trigger
and where the result goes differ.

### Hot add and hot reload

The IN-CONTAINER runner `fsnotify`-watches `/plugins` (the mounted plugins dir) and
reconciles desired-vs-running on a change (debounced, since an install writes
several files):

- a new plugin dir appears (staged by `/plugin add`, or dropped in) -> launch it +
  register its tools and slash command (HOT ADD), no container restart,
- a plugin dir is removed (`/plugin remove`) -> stop the plugin process and drop its
  command (HOT REMOVE),
- the runner skips hidden (dot-prefixed) dirs, so an install's brief hidden staging
  dir is ignored until the finished, non-hidden dir is renamed into place.

The watch lives in the RUNNER, not the host: the host only stages binaries into the
mounted dir, and the change propagates inward. This needs no host rebuild (plugins
are independent binaries) and no container restart. The host-side `/commands`
listing is refreshed in step by `/plugin add`/`remove` so the command is
discoverable immediately.

## Distribution

The launch path only ever needs a built binary plus a `plugin.yml` in a
`plugins/<name>/` subdir. Everything below is a front-end that produces exactly
that end state, so the launch core is built first and distribution is additive.

### All roads end at "a binary + plugin.yml in a plugin dir"

- baseline: the operator builds (or unpacks) the plugin into `plugins/<name>/`.
- `/plugin add <git-url>`: build-on-install. Clone the repo to a temp dir, `go
  build` its plugin command, and stage the resulting binary plus the repo's
  `plugin.yml` into `plugins/<name>/`. Requires the Go toolchain on the host.
- release pull: download a prebuilt binary + `plugin.yml` from a release into
  `plugins/<name>/`. No toolchain on the host.

### Build-on-install (the `/plugin add` mechanics)

`/plugin add <git-url>` runs the WHOLE clone + scan + build INSIDE a throwaway
container, so untrusted code never executes on the host (full threat model in
`docs/security.md`):

1. The host runs a one-off, rootless container from the runner image (which has git
   + the Go toolchain), mounting only a single staging dir at `/out`.
2. Inside that container: bare PUBLIC `git clone --depth 1` into `/work` (no creds,
   no proxy; a private URL fails fast); a red-flag scan (reject cgo, `//go:generate`,
   a `go.mod` `replace`; require a `goclawkit` import); then a pure-Go Linux build
   (`CGO_ENABLED=0 GOOS=linux go build -o /out/<exec>`), where `<exec>` is the
   `exec:` field from the repo's `plugin.yml`. The source commit is pinned.
3. The container writes ONLY the built binary, the `plugin.yml`, and the pinned
   commit to `/out`. On the host that is a staging area
   (`<data>/plugins-staging/`), separate from the watched plugins dir. The build
   itself happens in the container's `/work`, never in this dir.
4. The host copies just those files into `<data>/plugins/<name>/` (atomically, via a
   hidden rename; `<name>` is the manifest `name`). The runner's `fsnotify` watch
   then loads the new plugin live, no host or container restart. A failed build
   leaves nothing behind and reports the reason.

Only the PLUGIN is compiled (a small binary), never the host, and even that
compilation is sandboxed. The Go toolchain requirement is met by the runner image;
a release-pull path (download a prebuilt binary) could serve hosts that want to skip
the build entirely.

### The `/plugin` command set

A built-in `/plugin` command (owner-only) with subcommands:

- `add <git-url>`: sandboxed build-on-install as above; the plugin loads live.
- `list`: show installed plugins (name, version, author, and the slash command each
  registers), read from each `plugin.yml` under the plugins dir.
- `remove <name>`: delete the plugin's dir; the runner's watch stops it and the host
  drops its `/commands` listing.

(An enable/disable-without-removing toggle, an `update <name>` that re-clones and
rebuilds, and private-repo installs are noted as optional follow-ups, not built.)

### Optional later: a marketplace as data, not an engine

When there are more plugins than a handful, a simple registry maps a short name to
a source URL so `/plugin add <name>` need not paste a full URL:

```yaml
# marketplace.yml (a plain list; NOT a custom plugin engine)
plugins:
  roll:    { git: https://github.com/shindakun/roll,    description: Dice roller. }
  weather: { git: https://github.com/someone/gw-weather, description: Weather lookup. }
```

`/plugin add weather` resolves `weather` to its `git:` URL and runs the same
build-on-install path. This is deliberately just a lookup table; it adds discovery,
not a new install mechanism, and never couples the host to anyone else's plugin
framework. Defer it until a second plugin exists.

### Why build-on-install of the plugin, not the host

The thing built on install is the PLUGIN: a small, isolated binary staged into its
own `plugins/<name>/` dir. The host is never rebuilt. The install is a dropped-in
binary, not a source change, and plugins run as crash-isolated subprocesses. That
is the whole point of the subprocess model and the reason "add a plugin without
rebuilding or restarting the host" holds.

## Where plugins run: in the container (IMPLEMENTED)

Plugins now run INSIDE the agent container, not on the host. This section records
the motivation and the shape; it is built, not proposed.

### Why not host-side

A plugin is untrusted code: it is downloaded from a git URL, compiled, and run. If
the host launched it (an early version did, via `exec.CommandContext` on the host),
that untrusted code would run as the HOST USER, with the host's filesystem, network,
and environment (which holds real credentials). For stranger-authored code that is
the worst possible place to run it. The crash isolation a subprocess gives is real,
but it is not a security boundary: a malicious plugin host-side can read your files
and exfiltrate secrets. So plugins do not run on the host.

### The move: run plugins INSIDE the agent container

goclaw already runs the agent in a Podman container that is rootless,
`--user 1000:1000`, has no view of the host filesystem beyond explicit mounts, and
dies cleanly. Running plugins there puts untrusted code inside that same sandbox
instead of on the host. A malicious plugin can then touch only what the container
can (its mounts: `/sessions`, `/vault`, a mounted `/plugins`), never the host.

The architecture, mirroring the host-side model one level in:

- The host stages plugin binaries into a directory it mounts into the container
  read-only (host `<data>/plugins` mounted at `/plugins`, alongside `/sessions` and
  `/vault`). Installing or removing a plugin (`/plugin add`/`remove`) stays a HOST
  operation: the host owns what lands in that dir. The host never executes the
  plugin.
- The RUNNER (`cmd/claude-runner`, already a long-running loop in the container)
  becomes the in-container plugin manager. It does exactly what the host manager
  does now, one level in: walk `/plugins`, read each `plugin.yml`, launch the
  enabled plugin binaries as child processes, and speak the same frame protocol to
  them. No new mechanism, the same walk-and-load.
- HOT RELOAD does not require restarting the container. The runner runs its own
  `fsnotify` watch on `/plugins`, just like the host does on its `plugins/` dir.
  When the host drops in, removes, or toggles a plugin, the watcher in the runner
  launches or stops that plugin subprocess. The container keeps running; only the
  plugin processes inside it cycle. (This was the apparent objection to in-container
  plugins, and it dissolves: the manager moves into the runner, the watcher moves
  with it.)

### The agent path gets SIMPLER, not harder

The agent runs in the container; the plugins now run in the same container. So a
plugin tool is exposed to the agent as a LOCAL tool (an MCP/stdio tool the runner
registers with `claude.Query`, the SDK already supports MCP servers). The agent
calls it in-process to the container, with NO host boundary crossing. This DISSOLVES
the "agent tool bridge" open question: there is nothing to bridge, because the plugin
is already on the agent's side. The runner translates an MCP tool call into the
plugin's `tool.invoke` frame and returns the result.

### The cost: the slash-command path crosses inward

The one thing that gets harder is the user slash command. Today `/roll 2d6` is pure
host-side: router -> host plugin -> reply, instant, no container needed. With the
plugin in the container, `/roll` must reach inward to the plugin. Options, in order
of preference:

- Route the slash command through the SAME inbound path a normal message takes (the
  host writes a `tool.invoke`-style request into the session, the runner dispatches
  it to the plugin and writes the result to outbound). Reuses the existing
  host<->container boundary (the two SQLite files); no new channel. Cost: a slash
  command now waits on the container the way a normal message does (a warm container
  is fast; the first one is the usual lazy-launch delay).
- Or keep a small host-side fast path only for a vetted/built-in subset, and let
  third-party plugin commands take the inward route. More moving parts; probably not
  worth it.

The trade is deliberate: a little slash-command latency in exchange for never
running stranger code as the host user. For untrusted plugins that is the right
call.

### Threat-model delta (summary)

| | host-side (an earlier version) | in-container (SHIPPED) |
|---|---|---|
| Plugin runs as | the host user | rootless `1000:1000` in the sandbox |
| Can read host filesystem | yes | no (only container mounts) |
| Can reach host env/secrets | yes | no (only what the container is given) |
| Blast radius of a malicious plugin | the whole host | the container's mounts (`/sessions`, `/vault`) |
| Agent tool bridge | a boundary to cross | gone (plugin is local to the agent) |
| Slash command | host-local, instant | crosses inward via the session DBs |
| Hot reload | host watcher relaunches | runner watcher relaunches (container stays up) |

This was a migration, not a rewrite: the wire protocol, `plugin.yml`, the manager
logic, and the `/plugin` command set are shared; the manager moved from the host
into the runner, and the host's job narrowed to staging binaries into the mounted
dir. The container is not a perfect sandbox (it is not a microVM), but "rootless
container with explicit mounts" is a large, real improvement over "runs as you on
the host" for code you downloaded and compiled.

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
`pkg/plugin` (Tool/Serve/ServeTool, channel stub), and `cmd/roll`.

1. DONE. The `Client` (`internal/plugin`): launch, handshake, invoke with ID
   correlation. The shared frame protocol the runner and host both use.
2. DONE. Slash-command path + `/commands`: `internal/command` registry; `/roll 2d6`
   dispatches to the plugin; Discord @mention stripping so commands work in
   channels.
3. DONE. Plugins run IN the agent container (security): the host mounts
   `<data>/plugins` read-only at `/plugins`; the in-container runner discovers,
   launches, and exposes them to the agent as local MCP tools (this dissolved the
   "agent tool bridge", the plugin is already on the agent's side). The runner
   `fsnotify`-watches `/plugins` and reconciles live, so an install/remove takes
   effect with no restart.
4. DONE. The `/plugin` command set (owner-only): `add <git-url>` / `list` /
   `remove <name>`. `add` clones + scans + builds entirely inside a throwaway
   sandbox container (untrusted code never builds on the host), pinning the source
   commit; only the verified Linux binary + `plugin.yml` are staged into
   `<data>/plugins/<name>/`, where the runner's watch loads it. See "Distribution"
   and `docs/security.md`.
5. The `channel` kind: the SDK `ServeChannel` runtime, the `channel.*` topics, the
   host-side `ChannelAdapter` shim, and a reference channel plugin.
6. Optional: a marketplace lookup table (name -> git URL) so `/plugin add <name>`
   need not paste a URL. Data, not an engine; defer until a second plugin exists.
7. Optional: enable/disable without removing (a host-owned sidecar flag), and
   private-repo installs (inject a stored token into the in-container clone).
8. Layer 2 (only when a plugin needs cross-plugin or plugin-to-host coordination):
   a socket-backed `Transport` plus a host-side hub/registry routing
   topic-addressed frames between plugins, reusing the existing frame format. No
   change to Layer 1 or the wire format.

## Open questions

- Agent tool bridge: RESOLVED. Moving plugins into the agent container dissolved it,
  the plugin is on the agent's side, so its tools are local MCP tools with no
  boundary to cross.
- Supervision policy: when a plugin process exits unexpectedly, relaunch forever
  with backoff, or give up after N crashes and mark it failed? Currently it is
  dropped on exit and reloaded when its dir changes.
- Do plugins need the credential proxy? Tool plugins so far make their own outbound
  calls from inside the container, which already routes through the proxy. A plugin
  that needs a specific host secret is a future case.
