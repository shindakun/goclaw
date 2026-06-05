# Channel plugins: running an untrusted channel in the sandbox

Status: PARTIAL. The host<->channel-plugin PROTOCOL is built and proven LIVE: goclaw
launches the goclawkit IRC reference plugin (`goclawkit/cmd/irc`), it dials Libera over
TLS, joins a channel, and messages flow both ways through goclaw's host-side
`ChannelClient` (a real `#goclawtester` mention arrived as inbound and a reply posted
back). What is built so far is the DIRECT (no-container) path: `internal/plugin.
ChannelClient` (host half of `ServeChannel`), `kind: channel` accepted in the manifest,
the env allowlist (`Manifest.InjectEnv` / `MinimalEnvBase`), and a dev harness
(`cmd/chantest`). NOT yet built: the `channels.ChannelAdapter` wrapper that puts inbound
through the real router/agent (next), the sandboxed relay-over-socket boundary, and
hot-reload. See section 9 for the current position in the build order.

This doc covers how a third-party **channel** can be added to goclaw WITHOUT giving
untrusted code a foothold on the host. The deciding constraint and the whole reason this
doc exists: a channel needs a network front door, the sandbox cannot bind a hot-added
one, and we refuse to run untrusted code on the host.

## What a channel actually is (the contract, not the transport)

Before the split: a channel is defined by a small ADAPTER CONTRACT, not by how its
bytes arrive. goclaw's `channels.ChannelAdapter` is already exactly this contract:
`Name()`, `Start(ctx) -> <-chan InboundMsg` (the adapter PUSHES normalized inbound up
this channel), `Send(ctx, OutboundMsg)` (deliver one reply), plus the optional
`SendAction` (typing, ...). That is the whole definition. Notice what is NOT in it:
there is no `listen()`, no `poll()`, no `receive()`, no mention of a port, a socket, or
a direction. HOW a channel sources inbound, dial an upstream and read it, long-poll an
API, accept a webhook POST, read a local socket, is the adapter's private business; it
just calls `Start`'s stream when a message arrives and implements `Send` to reply. Any
transport that can do those two things is a channel.

So the inbound/outbound/local taxonomy below is NOT a property of channels. It is
GOCLAW'S OWN refinement, forced by a constraint a host-only system does not have: a
channel plugin here is UNTRUSTED DOWNLOADED CODE, and the container's mounts and ports
are frozen at launch (section 4.0). Because the contract is transport-agnostic but the
TRANSPORT is where the security and hot-add decisions live, goclaw has to classify
channels by where their transport runs, even though "what a channel is" stays the single
adapter contract above. The three shapes are three ways to satisfy one contract under
goclaw's sandbox rules.

The conclusion splits THREE ways by how the channel reaches the outside world
(sections 4, 6). The ordering is not arbitrary: surveying how real chat transports are
built, every platform that CAN be driven by dialing out IS driven that way (a long-poll
loop, a gateway websocket, a provider socket), and a public inbound listener is the
FALLBACK a platform forces on you only when it offers no outbound option at all. A
self-hosted bot behind NAT should prefer dialing out for exactly that reason. So:

- PRIMARY: an **outbound-dialer** channel (an IRC/Discord/Matrix/Telegram bridge that
  dials an upstream and holds the connection) IS a hot-added, sandbox-built plugin: a
  downloaded binary in the container behind a **host-side relay** (trusted, first-party).
  This is where untrusted third-party protocol code belongs, and it hot-adds cleanly
  because it needs no inbound port. This is the shape to build, and the one nearly every
  real chat platform supports.
- SECONDARY (fallback only): an **internet-inbound** channel (a public webhook receiver)
  is needed only for platforms that cannot dial out at all (some non-chat integrations:
  inbound-only webhooks, certain enterprise platforms). It is NOT a hot-added plugin: a
  hot-added container cannot get an externally-reachable port without a restart
  (section 4.0), so it is a **first-party host-ingress feature**. goclawkit's
  `cmd/webhook` survives as the SDK's protocol demo, not as an installed plugin.
- LOCAL: a **local-bridge** channel (an editor or CLI on the same machine talking over
  loopback or a Unix socket) is also a first-party feature, not a download: it binds a
  `127.0.0.1`/unix endpoint nothing external can reach. It is neither a sandboxed plugin
  nor public ingress; it is a trusted local endpoint the host owns (section 4c).

Either way the untrusted half sits BEHIND the boundary; the external front door (when
there is one at all) is always first-party host code.

Read `docs/plugins-design.md` first (the tool-plugin system this builds on) and
`docs/security.md` ("Plugins run in the sandbox") for the threat model. The
author-facing channel SDK already exists in goclawkit (`pkg/plugin/channel.go`,
`serve_channel.go`); the worked example is `goclawkit/cmd/webhook`.

## 1. Tool vs channel: why a channel cannot just copy the tool model

A tool plugin (roll) is **agent-initiated and self-contained in the sandbox**:

- The in-container runner (`cmd/claude-runner`) launches it, speaks the framed
  protocol over the child's stdio, exposes it to the agent as an MCP tool.
- It binds nothing, listens to nothing, reaches nothing outside the box. `tool.invoke`
  goes in, a result comes out. The container is its whole world. That is exactly why
  putting it in the sandbox cost us nothing: it never needed the host.

A channel plugin (a third-party IRC/Matrix/Slack bridge, or a webhook) is
**outside-world-initiated and inherently networked**:

- Messages arrive UNPROMPTED from outside (a websocket frame from a chat gateway, an
  IRC `PRIVMSG`, and in the rarer fallback case an HTTP POST). The channel's job is to
  carry that traffic, dialing out to the platform in the usual case.
- It is long-lived and bidirectional: it streams inbound up (`channel.inbound`
  events) for the life of the host, and accepts outbound down (`channel.send`
  requests) concurrently.
- It therefore wants to DIAL AN UPSTREAM (the normal case) or BIND A PORT (the fallback),
  which the agent container (rootless, no host network namespace, dies with the agent
  group) is the wrong place for inbound, and which is precisely the host's existing job:
  `internal/channels` (Telegram, Discord) runs on the host today.

So the naive move (drop a channel binary in `/plugins`, let the runner launch it like
a tool) fails twice: the sandbox cannot host the front door, and even if it could, a
channel is a host-router concern, not an agent-MCP concern. The agent never "calls" a
channel.

## 2. The two rejected alternatives, briefly

We considered and rejected:

- **Run channel plugins on the host, gated by vetting.** Treat a channel as a
  higher-trust class: stricter install gate (signature / allowlist / manual review),
  documented as "this runs on your host." REJECTED because it relocates the security
  boundary into operator judgment. The entire rest of goclaw is built assuming the
  host is trusted: the credential proxy keeps real tokens host-side, the mount
  allowlist guards host paths, the SQLite single-writer invariant is a host promise.
  One waved-through hostile binary runs as the host user with all of that, and none of
  it protects you, because none of it was designed to defend the host against itself.
  Vetting is one-mistake-fatal.
- **First-party webhook only (no third-party channel code at all).** Ship a single
  trusted inbound-webhook adapter on the host; "channel plugins" become config (a
  token + an outbound URL), never downloaded binaries. SAFEST and worth keeping as a
  fallback, but it abandons the goal: a third party cannot add a *new kind* of channel
  (a Matrix bridge, an SMS gateway) without us shipping host code for it. It answers a
  narrower question than the one asked.

The chosen design keeps untrusted channel code in the sandbox (structural boundary,
not judgment) while letting the host own the one thing the sandbox cannot: the socket.

## 3. The shim architecture

```
         OUTSIDE WORLD  (a webhook caller, a chat gateway, ...)
                  |  network  (TCP listen, or an outbound dial)
                  v
  ┌─────────────────────────────────────────────┐
  │ HOST: channel relay  (FIRST-PARTY, TRUSTED)  │
  │   - owns the socket / upstream connection     │
  │   - is a channels.ChannelAdapter to the router│
  │   - speaks the framed protocol to the plugin  │
  │     across "the boundary" (section 5)         │
  └───────────────────┬───────────────────────────┘
                      |  boundary: framed channel.* protocol
                      v
  ┌─────────────────────────────────────────────┐
  │ CONTAINER: channel plugin  (UNTRUSTED)        │
  │   the downloaded, sandbox-built binary        │
  │   ServeChannel():  Start() -> inbound events  │
  │                    Send()  <- outbound reqs   │
  │   same box as tool plugins; non-root; RO      │
  │   /plugins; no host fs/net; dies w/ container  │
  └─────────────────────────────────────────────┘
```

Two new pieces of FIRST-PARTY code, both on the host or in the runner (both trusted),
plus the boundary:

1. **Host channel relay** (`internal/channels/plugin/` or similar). For each installed
   `kind: channel` plugin it implements `channels.ChannelAdapter` (`Name`, `Start`,
   `Send`) and registers with the existing `channels.Registry`, so the router and
   delivery loop treat it identically to Telegram/Discord. Internally it speaks the
   framed `channel.*` protocol across the boundary to the in-container plugin:
   - `Start()` returns a `<-chan InboundMsg` fed by `channel.inbound` events read off
     the boundary.
   - `Send()` writes a `channel.send` request across the boundary and awaits the
     correlated result.
   The relay is the trusted half. It NEVER runs plugin code; it moves frames.

2. **In-container relay glue** (extends `cmd/claude-runner`). The runner already
   discovers `/plugins/<name>/` dirs and launches tool plugins as MCP servers. For a
   `kind: channel` plugin it instead launches the binary and bridges its stdio to the
   boundary, so the plugin's `ServeChannel` talks to the host relay. The plugin itself
   is unchanged from goclawkit's `ServeChannel` contract; the runner is what connects
   its stdio to the cross-boundary transport.

3. **The boundary** (section 5): the trusted byte pipe between host relay and
   in-container glue. This is the one new mechanism.

The untrusted binary sees only its own stdio (the `ServeChannel` it already
implements). It does not know or care that the "host" on the other end is actually a
relay reached across a container boundary. That is what makes the plugin code
identical whether it ran on the host or in the box.

IMPORTANT SCOPE NOTE: the diagram above is the OUTBOUND-DIALER shape (4a), the only
shape with an in-container plugin process, and the primary one. The other two shapes
have NO in-container binary: internet-inbound (4b) is a first-party host ingress and
local-bridge (4c) is a first-party loopback endpoint, both because section 4.0's
frozen-ports constraint means a hot-added listener cannot be reached inside the
container. The relay, boundary, and `ChannelAdapter` wiring are shared; the "downloaded
binary in the box" half applies only to 4a.

## 4. Who owns the front door? Three channel shapes

### 4.0. The constraint that drives everything: container mounts and ports are FROZEN

A podman container's mounts AND published ports are fixed at `podman run` time. There
is no "add a mount" or "publish a port" to an ALREADY-RUNNING container
(`internal/runtime/session.go`'s `EnsureGroupRunner` builds the whole mount set before
`Run`; nothing adds to it after). Hot-add means installing a channel WITHOUT restarting
the container. So:

- A hot-added channel can NEVER get a newly-published container port. `podman -p` is a
  run-time flag; using it for a hot-added inbound channel would require a container
  restart, which breaks the invariant the whole plugin system is built on.
- Anything that must be reachable from outside has to be held by something that ALREADY
  EXISTS before the channel is added: the host process (always up), or a port/mount
  fixed at container start.

The pattern that already dodges this is `/plugins`: it is mounted ONCE as a DIRECTORY,
and hot-add works because installing a plugin adds a FILE INSIDE the already-mounted dir
(fsnotify watches contents), never a new mount. Every dynamic boundary in this design
MUST follow that pattern: pre-mount a directory at container start, create per-channel
files inside it at hot-add time. A per-channel mount or per-channel published port is
forbidden because it cannot be done without a restart.

### 4a. Outbound-dialer channels (PRIMARY): the real hot-added plugin

The channel is a CLIENT: it dials an upstream and holds the connection (an IRC server, a
Discord/Matrix gateway websocket, a provider socket). This is the shape that genuinely
needs a long-lived plugin process, and it HOT-ADDS CLEANLY precisely because it needs no
inbound port: it dials OUT over the container's existing egress, so section 4.0's frozen
-ports problem never bites. This is where untrusted third-party protocol code belongs,
it is the shape that justifies a channel PLUGIN, and it is the shape nearly every real
chat platform supports (each is normally driven by a dial-out: a long-poll loop, a
gateway websocket, a provider socket). Build this one.

For this shape the question is WHERE the outbound dial happens:

- Option A: the plugin dials, in the container, IF the container is allowed egress to
  that upstream. The agent container already has controlled network egress (via the
  credential proxy for some hosts); a chat-gateway dial may or may not be permitted by
  the egress policy. If permitted, the plugin holds the upstream connection and the
  boundary only carries normalized inbound/outbound (NOT the raw gateway protocol).
  This keeps untrusted protocol-parsing in the box, which is the security win.
- Option B: the host relay dials and the plugin only normalizes. This pulls
  third-party protocol parsing back onto the host, which is most of what we were
  trying to avoid. Reject unless egress policy forbids A.

WORKED EXAMPLE: an IRC gateway. IRC is the cleanest dialer case and worth walking
because it exercises everything a stateless inbound POST never would.

- ONE dial-out, no listener. The plugin opens a single long-lived TCP connection to
  `irc.example.net:6697` and never binds anything (DCC, which does listen, is a side
  feature we never expose). So section 4.0's frozen-ports problem simply does not exist
  here: nothing inbound to publish. It hot-adds with zero ingress wiring.
- INBOUND and OUTBOUND share that one socket. Server `PRIVMSG` pushes become
  `channel.inbound` events; the agent's reply becomes a `channel.send` the plugin writes
  as a `PRIVMSG` back over the SAME connection. The boundary carries only normalized
  Inbound/Outbound, never raw IRC lines.
- STATEFUL and long-lived: the plugin's `Start()` holds the connection for hours,
  answers `PING` with `PONG` (or the server drops it), and owns reconnect/backoff and
  re-`JOIN` after a netsplit. The goclawkit `Channel` contract already delegates this
  ("the implementation owns reconnect/backoff", channel.go). This is what proves the
  boundary socket must stay up for the life of the container, not be torn down between
  messages.
- ChatID / identity mapping is REAL work here, and it is the plugin's job: a `PRIVMSG`
  to `#go-nuts` is a group message (ChatID = the channel name); a `PRIVMSG` to the bot's
  own nick is a DM (ChatID = the sender's nick). The IRC server ASSERTS the sender nick
  and does NOT authenticate it (absent SASL/NickServ), so per section 7 the relay
  namespaces it (`irc:<network>/<nick>`) and never trusts it as an owner id.

This is the example to BUILD first (section 9): no inbound port, so it isolates the
boundary + hot-reload + long-lived-connection mechanics with nothing else in the way.
Discord/Matrix/XMPP are the same shape with heavier protocols.

### 4b. Internet-inbound channels (SECONDARY, fallback only): host owns the ingress

The channel is a SERVER: something outside POSTs to it (a public webhook receiver). This
is needed ONLY for platforms that cannot dial out at all: inbound-only webhooks and
certain enterprise integrations that push events to a URL and offer no socket/poll to
pull from. For anything that can be driven by dialing out, prefer 4a; do not reach for an
inbound listener by default, it is the harder and rarer case.

The naive read is "the plugin binds a port inside the container, the host just publishes
it." Section 4.0 kills that for hot-add: a hot-added inbound channel cannot get a
published port without a container restart. So the externally-reachable listener CANNOT
live in the container; it must live on the host. The shape (how every real webhook
gateway works: one host, many paths) is ONE always-on host ingress, multiplexed by PATH:

- The host binds ONE public listener at startup, fixed forever (e.g. `:8080`).
- A hot-added inbound channel gets a ROUTE, not a port: `POST /channels/<name>/inbound`.
  Adding a channel adds an in-memory route to the host's mux (instant, no port, no
  mount, no restart). Removing it drops the route.
- The host relay authenticates the POST, namespaces identity (section 7), and emits the
  normalized inbound directly.

In this model the HOST relay holds the listener and does auth + identity, so there is no
untrusted process in the inbound path. A pure inbound webhook therefore does not need a
plugin process at all: it is a FIRST-PARTY HOST FEATURE configured with a token + an
outbound target. goclawkit's `cmd/webhook` survives as the SDK's protocol demo (and its
`-selftest`), not as a thing you `/plugin add`. This is also a SECURITY GAIN: untrusted
code never owns the external front door; the front door is always first-party host code.

### 4c. Local-bridge channels (LOCAL): a first-party loopback endpoint

The channel is an editor or CLI ON THE SAME MACHINE talking to the host over loopback
(`127.0.0.1:<port>`) or a Unix socket. Nothing external can reach it, so it needs no
public ingress and no published container port. Like 4b it is a FIRST-PARTY host feature,
not a download: the host binds the local endpoint, authenticates (a bearer token is
typical even on loopback), and registers a `ChannelAdapter`. The "untrusted plugin in the
box" model does not apply because there is no untrusted code: this is host-owned code
talking to a trusted local process. Treat it like the credential proxy: a built-in the
host stands up, not something installed from a git URL.

## 5. The boundary: Unix socket vs the SQLite pair

The host relay and the in-container glue need a bidirectional, long-lived, framed byte
pipe. Two candidates; this is the decision the doc was written to support.

### 5a. Unix domain socket in a PRE-MOUNTED socket dir (RECOMMENDED)

A single socket DIRECTORY is mounted into the container ONCE at `podman run` time
(section 4.0: a per-channel mount is forbidden, it cannot be added to a running
container). Per-channel SOCKET FILES are created INSIDE that already-mounted dir at
hot-add time, exactly as plugins are files inside the already-mounted `/plugins`.

```
mount (once, at container start):
           <dataDir>/run/channels/   ->  /run/goclaw/channels/   (read-write)

per channel (hot-add, no restart, just a new file in the mounted dir):
host:      net.Listen("unix", <dataDir>/run/channels/<name>.sock)
runner:    net.Dial("unix", "/run/goclaw/channels/<name>.sock") <-> plugin stdio
```

- This is LITERALLY what `goclawkit/pkg/ipc`'s `Transport` abstraction was designed
  to enable: "A later Layer 2 socket bus would provide a Dial/Listen-based Transport
  carrying the same frames, so it is purely additive: a new Transport, not a new wire
  format" (ipc/proto.go:78-84). The frames are unchanged; only the Transport differs.
- True duplex stream: a channel is long-lived and low-latency (a chat reply should not
  wait on a poll interval). A socket is the right shape.
- HOT-ADD SAFE: because the DIR is mounted once and the per-channel socket is just a
  file inside it, adding/removing a channel never touches the container's frozen mount
  set. The in-container runner watches the dir (it already watches `/plugins`) and dials
  a new socket when it appears. Lifecycle: the relay creates the socket on channel add,
  unlinks it on remove.
- COST: ONE new mount (the dir), not one per channel. It is a host-writable path the
  container can also write (a duplex pipe), which is a NEW kind of mount: all current
  mounts are either RO (`/plugins`, CA cert) or the single-writer-per-file SQLite pair.
  A read-write socket dir the container writes is outside the "two SQLite files, one
  writer each" invariant. That is not a violation (the invariant is about the
  inbound/outbound DBs specifically), but it IS a new trusted channel into the host
  relay, and the relay must treat everything arriving on it as untrusted input (frame
  caps already enforce 8 MiB / 255-byte-topic bounds; the relay must still validate
  inbound identity, section 7).

### 5b. Reuse the inbound.db / outbound.db SQLite pair

Carry `channel.inbound` as rows the plugin writes to outbound.db (host reads RO) and
`channel.send` as rows the host writes to inbound.db (plugin reads).

- PRESERVES the invariant verbatim: still exactly two SQLite files, host writes
  inbound, container writes outbound, host opens outbound RO. No new mount, no new
  mechanism.
- But the DB pair is REQUEST-BATCH shaped (a message in, a reply out, ledgered),
  whereas a channel is a STREAM. Cramming a long-lived duplex channel transport into a
  ledger means: polling latency on both directions (a chat reply waits for the next
  poll tick), and the outbound.db single-writer rule now contends between the agent's
  normal replies AND the channel plugin's inbound events, since both would be the
  "container writes outbound" side. That muddies the one invariant that "explains
  everything" (root CLAUDE.md). The ledger was built to durably track delivery, not to
  be a message bus.
- VERDICT: technically possible, conceptually wrong. The SQLite pair is the
  agent<->host conversation boundary; a channel is a DIFFERENT boundary (outside-world
  <-> host) that happens to pass through the container. Overloading the agent boundary
  to carry channel transport coupes two unrelated concerns and pays latency for it.

### Recommendation: 5a (Unix socket).

It is what the SDK's Transport abstraction anticipated, it is stream-shaped for a
stream problem, it keeps the SQLite invariant clean by NOT touching it, and (crucially
for hot-add) it adds ONE mount, the socket dir, with per-channel sockets as files
inside it. The relay treating socket input as untrusted is the same posture we already
hold for every other input.

## 6. What changes in the goclawkit SDK and the webhook example

This is the crux of "does the worked example still work, and does the SDK need to
change." Short answer: **the SDK needs NO wire change and `-selftest` keeps working
untouched. The webhook PLUGIN, as a thing you `/plugin add`, does not fit the hot-add
inbound case (section 4.0): its in-container listener cannot be reached from outside
without a container restart. So the production inbound webhook becomes a FIRST-PARTY
HOST FEATURE (host ingress, section 4b), and goclawkit's `cmd/webhook` survives as the
SDK's protocol demo, not as an installable plugin.**

### 6a. The goclawkit SDK: no wire/protocol change needed

- The frame protocol (`pkg/ipc`) is unchanged: same four frame types, same header. A
  Unix-socket boundary is a new `Transport`, which `ipc` already supports by design.
- The channel topics (`channel.inbound`, `channel.send`, `channel.action`) are already
  defined and frozen in `serve_channel.go`. The relay speaks exactly these.
- `ServeChannel` is unchanged: it reads/writes frames over `ipc.StdioTransport` (the
  plugin's stdin/stdout). In the shim model the IN-CONTAINER RUNNER connects the
  plugin's stdio to the Unix socket; the plugin still just does stdio. So a channel
  plugin author writes the SAME `ServeChannel(ch)` they write today.

### 6b. Why the webhook cannot be a hot-added plugin (and what it is instead)

The tempting story is "the plugin binds `WEBHOOK_ADDR` inside the container, that bind
succeeds, the host just makes it reachable." The bind DOES succeed (a port in the
container's network namespace), but "make it reachable" is the trap: section 4.0 says a
hot-added channel can NEVER get a published container port without a restart. The
outside world cannot reach an in-container listener that was added after the container
started. So for a webhook installed AT RUNTIME, the in-container listener is unreachable
from outside, full stop. The plugin's `Start()` HTTP listener cannot be the front door.

That is why inbound webhook resolves to a FIRST-PARTY HOST FEATURE, not an installable
plugin (section 4b): the host owns one always-on ingress and routes `/channels/<name>/
inbound` to a host relay that authenticates, namespaces identity, and forwards the
normalized inbound across the pre-mounted socket dir. In that model the host does the
listening and the auth, so the webhook plugin's `Start()` listener and `authOK` are not
used. A pure inbound webhook therefore does not need an in-container process at all.

Consequence for `cmd/webhook`: it stays in goclawkit as the worked SDK demo of the
`channel.*` protocol (handshake, `channel.inbound` event, `channel.send` request, the
wire test, `-selftest`). It is NOT the thing a goclaw operator `/plugin add`s for a
production webhook; that path is the first-party host ingress. The two are not in
conflict: the SDK demo proves the protocol; the host feature ships the capability.

If we later DID want a webhook as an installed plugin (e.g. an operator who pre-declares
it before container start, accepting a restart to add it), reachability via a published
port is possible but explicitly OUT of the hot-add story. Default: do not. Section 10
holds this as the one open product call.

### 6c. `-selftest` is unaffected

`-selftest` (`selftest.go`) never calls `Start()` and never binds the real port: it
constructs the channel, calls `decodeInbound` directly, then `Send` to an in-process
`httptest` sink, and prints the round trip. It has NO dependency on where the plugin
runs or how the boundary is carried, so the shim changes nothing about it. It keeps
working verbatim (verified: `go run ./cmd/webhook -selftest` prints the inbound and the
delivered outbound and exits 0). Whatever we do host-side, `-selftest` stays the
no-host local smoke test it is today.

### 6d. The dialer (4a) is what the SDK's channel side is FOR

The outbound dialer (section 4a) is the shape that justifies a channel PLUGIN: untrusted
third-party protocol code we want in the box. `cmd/webhook` does not exercise it (it is a
listener, not a dialer), so goclawkit ships a SECOND worked example that does:
`goclawkit/cmd/irc`, a minimal IRC bridge (dials out over TLS, joins, relays mentions and
DMs). It proves the dialer path end to end and is the build goclaw's `ChannelClient` was
validated against (PROVEN LIVE on Libera). `ServeChannel` supported it with no wire
change, exactly as predicted: the channel SDK side needed nothing new.

## 7. Security review

The shim's security claim: untrusted channel code runs ONLY in the sandbox; the host
gains ONLY a byte-moving relay it fully controls.

- IDENTITY IS NOT THE PLUGIN'S TO ASSERT. The webhook example already gets this right
  (`decodeInbound` namespaces `webhook:<sender>` and never trusts the body's id as the
  access-gate SenderID). The relay MUST enforce this regardless of what the plugin
  sends: an `Inbound` arriving across the boundary has its `SenderID` namespaced /
  validated by the RELAY before it reaches the router's access gate, so a hostile
  plugin cannot forge an owner id. Fail closed: a malformed inbound is dropped.
- THE HOST STILL APPLIES ITS OWN ACCESS GATE. The relay authenticates the transport and
  pins identity; the router authorizes the resulting sender (`internal/permissions`),
  exactly as for Telegram/Discord. Defense in depth: the plugin is not trusted to
  authorize anything.
- THE BOUNDARY IS UNTRUSTED INPUT. Everything the relay reads off the socket is from
  untrusted code. Frame caps (8 MiB payload, 255-byte topic) already bound it; the
  relay additionally validates payload shape per topic and never executes anything the
  plugin says beyond "emit this normalized inbound" / "the send succeeded/failed."
- BLAST RADIUS UNCHANGED FROM TOOL PLUGINS. A hostile channel plugin can: lie about
  inbound contents (it is the transport, it always could), refuse to send, or spam
  inbound events (rate-limited by the relay). It CANNOT: bind a host port, read host
  secrets (credential proxy keeps them host-side), touch the host filesystem, or
  outlive its container. Same box, same limits as roll.
- INSTALL PIPELINE UNCHANGED. A channel plugin installs through the SAME sandboxed
  build (`internal/plugin/install.go`): clone + red-flag scan + Linux build in a
  throwaway container, only the verified binary + plugin.yml leave. `kind: channel`
  is just a manifest value; the build does not care. The host never builds or runs the
  plugin during install (section "Installing a plugin" in security.md holds verbatim).
- THE NEW MOUNT IS THE ONLY NEW SURFACE. One socket DIRECTORY is mounted read-write
  (section 5a); the container can write it, unlike every existing mount. Per-channel
  sockets are files inside it, owned by the relay, carrying only framed bytes the relay
  treats as untrusted. It does NOT touch the inbound.db/outbound.db invariant (that pair
  is untouched). Each socket is created with tight permissions (0600, host user) and
  unlinked on channel removal; the dir itself is created once.
- THE EXTERNAL FRONT DOOR IS ALWAYS FIRST-PARTY. For internet-inbound channels (section
  4b) the host owns the always-on ingress and the auth; untrusted code never binds the
  externally-reachable port. For outbound dialers (4a) there is no inbound port at all.
  Either way the untrusted plugin is behind the boundary, never in front of it.

## 8. Lifecycle and hot-reload

The shapes hot-add differently, because only the outbound dialer is an installed plugin
PROCESS (section 6b); internet-inbound and local-bridge are first-party host features.

OUTBOUND-DIALER CHANNELS (4a), the real channel plugin:

- A `kind: channel` dir appears in `data/plugins/<name>/` (installed via `/plugin add`
  exactly like a tool). The host (NOT just the runner) must learn about it, because the
  relay is host-side. Host-side discovery walks the plugins dir for `kind: channel`
  manifests and, for each, creates a relay, a socket FILE in the pre-mounted socket dir
  (section 5a, no new mount), and registers a `ChannelAdapter`.
- The runner's existing fsnotify watch launches the in-container plugin process and
  connects its stdio to the channel socket. The plugin dials its upstream; inbound and
  outbound flow across the socket.
- Hot-add: a new channel dir -> host creates relay + socket file + adapter, runner
  launches the process, they meet on the socket. Hot-remove: dir gone -> runner stops
  the process, host unregisters the adapter (`channels.Registry.Unregister`) and unlinks
  the socket file. `Unregister` exists (drops the registry entry, reports whether one
  was present, idempotent, freed name re-registerable on reinstall). It only removes the
  entry: stopping the adapter's `Start` goroutine and unlinking its socket is the
  relay's job, since the registry does not own those.
- ORDER/RACE: the relay tolerates the plugin not being connected yet (socket bound,
  nothing dialed) and the plugin tolerates the socket not being ready (retry the dial).
  Same "launch is lazy / first message is slower" posture the container already has.

INTERNET-INBOUND CHANNELS (4b) and LOCAL-BRIDGE CHANNELS (4c), the first-party features:

- These are NOT in-container plugin processes. Hot-add is even simpler and never touches
  the container: for 4b the host adds a ROUTE (`/channels/<name>/inbound`) to its
  always-on ingress mux and registers a `ChannelAdapter`; for 4c the host binds a
  loopback/unix endpoint and registers a `ChannelAdapter`. Hot-remove drops the route /
  closes the endpoint and unregisters. No socket file, no plugin process, no `/plugins`
  dir involvement.
- Config (channel name, token, outbound/local target) is host-side state, not a
  downloaded artifact. Adding/removing one is a host config change, applied live.

## 9. Build order (with current status)

Prove the host<->plugin PROTOCOL on the direct (no-container) path FIRST, with the
OUTBOUND DIALER (the real hot-add plugin, needs no ingress, the shape nearly every chat
platform supports), THEN slide the sandboxed boundary underneath it. Building the
boundary first would have meant debugging two new things at once; proving the protocol
against a real server first means the boundary is the only unknown when we add it.

1. DONE. Accept `kind: channel` in the manifest (`internal/plugin/manifest.go`); a
   channel has no `command`, lists its env var NAMES, declares `kind: channel`.
2. DONE. Env allowlist: `Manifest.InjectEnv` + `MinimalEnvBase` hand a plugin ONLY its
   declared env: names on a PATH-only base, never the host env (section 6/7). The
   tool-plugin launcher leak (bare os.Environ()) was fixed in the same change.
3. DONE. Host channel client: `internal/plugin.ChannelClient`, the host half of
   goclawkit's `ServeChannel`. Handshake asserting kind=channel, a read loop surfacing
   `channel.inbound` as a Go channel, `SendOutbound` writing `channel.send` correlated
   by frame id.
4. DONE. Dev harness `cmd/chantest`: launches a channel plugin DIRECTLY (no container,
   no socket, no agent), prints inbound, optionally echoes. PROVEN LIVE against the
   goclawkit IRC plugin: dialed Libera over TLS, joined `#goclawtester`, a real mention
   arrived as inbound and a reply posted back. This isolates the channel.* protocol with
   the boundary still absent, which is exactly the point.
5. NEXT. `channels.ChannelAdapter` wrapper over `ChannelClient`: `Start()` returns the
   inbound stream mapped to `channels.InboundMsg`; `Send()` calls `SendOutbound`. Register
   it in `channels.Registry` so the real ROUTER and AGENT see IRC messages (today chantest
   prints them; this routes them). Enforce identity namespacing (`irc:<network>/<nick>`,
   section 7) at the mapping boundary. The two message types are field-for-field aligned,
   so this is thin.
6. THEN. The sandboxed boundary (sections 4.0, 5a): pre-mounted socket dir mounted once at
   container start, the in-container relay glue that pipes a per-channel socket file <->
   plugin stdio, so the SAME `ChannelClient` reaches a plugin running in the box instead of
   on the host. Plus host-side `kind: channel` discovery and hot-reload (section 8);
   `channels.Registry.Unregister` is already in place for hot-remove.
7. (Only if a target platform forces it) Internet-inbound host ingress (section 4b): one
   always-on listener, `/channels/<name>/inbound` routes, host-side auth + identity. The
   fallback inbound webhook, a FIRST-PARTY feature; goclawkit's `cmd/webhook` is the SDK
   protocol demo, not an installed plugin. Local-bridge (4c) is the same first-party
   pattern on a loopback/unix endpoint.

## 10. Open questions to resolve during build

- Container egress for 4a dialers: HOST egress is proven (the IRC plugin dialed Libera
  over TLS from the host via chantest). The open question is the CONTAINER: does the
  agent container's egress policy permit a chat-gateway dial, or must specific upstreams
  be allowlisted (like the credential proxy's per-host injection)? Determines whether 4a
  option A (plugin dials, in the box) is viable once the sandboxed boundary lands.
- RESOLVED (section 4.0 / 6b): a pure inbound webhook is a FIRST-PARTY host-ingress
  feature, not a hot-added plugin, because a hot-added channel cannot get an externally
  -reachable container port without a restart. It is also a FALLBACK shape, reached only
  when a platform cannot be driven by dialing out. goclawkit's `cmd/webhook` is the SDK
  protocol demo. The only remaining sub-question: do we EVER support a webhook as a
  pre-declared (restart-required, not hot-added) installed plugin? Default no.
- Pre-mounted socket dir on rootless podman: SELinux `:Z` relabel interplay with a
  read-write DIR holding live Unix sockets the container both reads and writes, and
  confirming the runner's fsnotify sees socket files appear/disappear inside it the way
  it sees plugin dirs under `/plugins`.
