# Channel plugins: running an untrusted channel in the sandbox

Status: DESIGN (nothing here is built yet). This doc covers how a third-party
**channel** plugin can be added to goclaw the same way a tool plugin is (a
downloaded, sandbox-built binary, hot-reloadable, no host rebuild), WITHOUT giving
that untrusted binary a foothold on the host. The deciding constraint and the whole
reason this doc exists: a channel needs a network front door, the sandbox cannot
bind one, and we refuse to run untrusted code on the host. The answer is a
**host-side relay** (trusted, first-party) in front of an **in-container channel
plugin** (untrusted, sandboxed).

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

A channel plugin (webhook, or a hypothetical third-party Slack/Matrix/IRC bridge) is
**outside-world-initiated and inherently networked**:

- Messages arrive UNPROMPTED from outside (an HTTP POST, a websocket frame from a
  chat gateway). The plugin's job is to be the front door.
- It is long-lived and bidirectional: it streams inbound up (`channel.inbound`
  events) for the life of the host, and accepts outbound down (`channel.send`
  requests) concurrently.
- It therefore wants to BIND A PORT or DIAL AN UPSTREAM, which the agent container
  (rootless, no host network namespace, dies with the agent group) is the wrong
  place for, and which is precisely the host's existing job: `internal/channels`
  (Telegram, Discord) runs on the host today.

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

## 4. Who owns the front door? Two channel sub-shapes

"A channel binds a port" hides a fork, and it changes how much the webhook plugin
must change (section 6). There are two sub-shapes:

### 4a. Inbound-listener channels (the webhook shape)

The channel is a SERVER: something outside POSTs to it. Today `cmd/webhook` binds
`WEBHOOK_ADDR` ITSELF inside `Start()`. In the shim model that port must be bound by
the HOST relay (the sandbox cannot bind a host-reachable port), not the plugin. So
for this shape:

- The HOST relay owns the listener. An inbound POST arriving at the host becomes a
  `channel.send`-style... NO: it becomes an INBOUND. The relay decodes the POST into
  an `InboundMsg` and emits it on `Start()`'s channel. The plugin's role shrinks to
  "decode/normalize," which is logic, not a socket.
- This means a pure inbound-webhook channel barely needs the plugin process at all:
  the interesting code (auth check, identity namespacing, JSON shape) could live in
  the relay. See section 6 for the tension this creates with the existing webhook
  plugin, and the recommendation.

### 4b. Outbound-dialer channels (the chat-gateway shape)

The channel is a CLIENT: it dials an upstream and holds the connection (Discord
gateway websocket, an IRC server, an XMPP server). This is the shape that genuinely
needs a long-lived plugin process, and the shape vetting-vs-shim actually matters for,
because here the plugin must speak a real third-party protocol we do not want to build
into the host.

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

The recommendation (section 9) is: design the boundary and relay so BOTH sub-shapes
work, but build 4a (inbound webhook) first because it is the worked example and needs
no container egress, and treat 4b as the validating second case.

## 5. The boundary: Unix socket vs the SQLite pair

The host relay and the in-container glue need a bidirectional, long-lived, framed byte
pipe. Two candidates; this is the decision the doc was written to support.

### 5a. Unix domain socket mounted into the container (RECOMMENDED)

The host binds a Unix socket at a host path that is mounted into the container; the
in-container runner dials it and pipes the framed protocol between the socket and the
plugin's stdio.

```
host:      net.Listen("unix", <dataDir>/run/chan-<name>.sock)
mount:     that file (or its dir) mounted into the container at /run/goclaw/chan-<name>.sock
runner:    net.Dial("unix", "/run/goclaw/chan-<name>.sock") <-> plugin stdio
```

- This is LITERALLY what `goclawkit/pkg/ipc`'s `Transport` abstraction was designed
  to enable: "A later Layer 2 socket bus would provide a Dial/Listen-based Transport
  carrying the same frames, so it is purely additive: a new Transport, not a new wire
  format" (ipc/proto.go:78-84). The frames are unchanged; only the Transport differs.
- True duplex stream: a channel is long-lived and low-latency (a chat reply should not
  wait on a poll interval). A socket is the right shape.
- Lifecycle: the relay owns the socket file (create on start, unlink on stop). Per
  channel, one socket. Rootless podman can bind-mount a host path into the container;
  we already do this for `/plugins`, `/vault`, `/sessions`.
- COST: a new mount and socket lifecycle to manage. The socket is a host-writable path
  the container can also write (it is a duplex pipe), which is a NEW kind of mount: all
  current mounts are either RO (`/plugins`, CA cert) or a single-writer-per-file SQLite
  pair. A read-write socket the container writes is outside the "two SQLite files, one
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
stream problem, and it keeps the SQLite invariant clean by NOT touching it. The cost
(one new mounted socket per channel, with its lifecycle) is real but contained, and
the relay treating socket input as untrusted is the same posture we already hold for
every other input.

## 6. What changes in the webhook plugin (and the goclawkit SDK)

This is the crux of "does the worked example still work, and does the SDK need to
change." Short answer: **the SDK needs NO wire change; the channel sub-shape (4a vs
4b) decides whether the webhook plugin changes a lot or a little.**

### 6a. The goclawkit SDK: no wire/protocol change needed

- The frame protocol (`pkg/ipc`) is unchanged: same four frame types, same header. A
  Unix-socket boundary is a new `Transport`, which `ipc` already supports by design.
- The channel topics (`channel.inbound`, `channel.send`, `channel.action`) are already
  defined and frozen in `serve_channel.go`. The relay speaks exactly these.
- `ServeChannel` is unchanged: it reads/writes frames over `ipc.StdioTransport` (the
  plugin's stdin/stdout). In the shim model the IN-CONTAINER RUNNER connects the
  plugin's stdio to the Unix socket; the plugin still just does stdio. So a channel
  plugin author writes the SAME `ServeChannel(ch)` they write today.

The one thing worth ADDING to the SDK is documentation/guidance, not code: a channel
plugin SHOULD NOT bind its own host-reachable port in `Start()` if it wants to run in
the goclaw sandbox, because in the sandbox there is no host-reachable port to bind.
That guidance distinguishes the two sub-shapes for plugin authors.

### 6b. The webhook plugin specifically (sub-shape 4a, inbound listener)

The webhook plugin as written binds `WEBHOOK_ADDR` itself inside `Start()`
(`main.go:113-118`, `listen(c.addr)`). In the sandbox that bind either fails (no
permission / not host-reachable) or binds a container-internal port nothing outside can
reach. So the webhook plugin DOES need to change for the shim model, and there are two
ways to resolve it:

- RESOLUTION 1 (relay owns the listener): the HOST relay binds the inbound HTTP
  listener; an inbound POST is decoded by the relay (or forwarded to the plugin as a
  `channel.inbound`-shaped... no, inbound flows plugin->host, so the relay would just
  emit the InboundMsg directly). Under this resolution the webhook plugin's `Start()`
  listener is DEAD CODE in the sandbox: the plugin reduces to a normalizer, and a pure
  inbound webhook arguably does not need a plugin process at all (it could be a relay
  config). This is the cleanest security story (no untrusted process in the inbound
  path) but it means "webhook" is really a first-party relay feature, not a plugin.
- RESOLUTION 2 (plugin keeps the listener, host forwards to it): the host relay binds
  the PUBLIC port, and forwards each inbound POST across the boundary to the plugin's
  IN-CONTAINER listener, then reads the resulting `channel.inbound`. This keeps the
  plugin's decode/auth/normalize logic untrusted-in-the-box, but it is a double hop
  (host port -> boundary -> plugin listener -> boundary -> host) for an inbound that
  the relay already holds the bytes of. Mostly pointless for the webhook shape.

For the INBOUND-LISTENER shape (4a), resolution 1 is right: the host relay owns the
listener and the "plugin" is thin enough to question whether it should be a plugin.
This means the webhook EXAMPLE is best understood as the SDK demonstrating the
`channel.*` protocol end to end, while the PRODUCTION inbound-webhook in goclaw is a
first-party relay feature. The third-party-plugin value lives in sub-shape 4b.

### 6c. The shape that justifies a channel PLUGIN: 4b (outbound dialer)

A Discord/Matrix/IRC bridge dials an upstream and parses a real third-party protocol.
THAT is untrusted code we genuinely want in the box and genuinely cannot reasonably
ship first-party for every chat network. For 4b:

- The plugin's `Start()` dials the upstream (in-container, if egress allows) and emits
  normalized `Inbound` for each upstream message. `Send()` pushes outbound to the
  upstream. The boundary carries ONLY normalized inbound/outbound, never the raw
  gateway protocol.
- The webhook plugin does NOT exercise this shape (it is a listener, not a dialer), so
  a SECOND worked example in goclawkit would be valuable: a minimal outbound-dialer
  channel (even one that dials a localhost echo upstream) to prove the 4b path. That is
  an SDK addition (a new `cmd/<example>`), not a wire change.

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
- THE NEW MOUNT IS THE ONLY NEW SURFACE. The Unix socket is a read-write path the
  container can write, unlike every existing mount. It is per-channel, owned by the
  relay, and carries only framed bytes the relay treats as untrusted. It does NOT touch
  the inbound.db/outbound.db invariant (that pair is untouched). It must be created
  with tight permissions (0600, owned by the host user) and unlinked on stop.

## 8. Lifecycle and hot-reload

Channels reuse the tool-plugin discovery/hot-reload machinery, extended:

- A `kind: channel` dir appears in `data/plugins/<name>/` (installed via `/plugin add`
  exactly like a tool). The host (NOT just the runner) must learn about it, because the
  relay is host-side. So host-side discovery walks the plugins dir for `kind: channel`
  manifests and, for each, creates a relay + socket + registers a `ChannelAdapter`.
- The runner's existing fsnotify watch launches the in-container plugin process and
  connects its stdio to the channel socket.
- Hot-add: a new channel dir -> host creates relay/socket/adapter, runner launches the
  process, they meet on the socket. Hot-remove: dir gone -> runner stops the process,
  host unregisters the adapter (`channels.Registry.Unregister`) and unlinks the socket.
  `Unregister` exists (it drops the registry entry and reports whether one was present;
  it is idempotent and the freed name can be re-registered on reinstall). It only
  removes the entry: stopping the adapter's `Start` goroutine and unlinking its socket
  is the relay's job, since the registry does not own those.
- ORDER/RACE: the relay should tolerate the plugin not being connected yet (socket
  bound, nothing dialed) and the plugin should tolerate the socket not being ready
  (retry the dial). Same "launch is lazy / first message is slower" posture the
  container already has.

## 9. Recommended build order

1. Boundary first: a Unix-socket `Transport` on the host side (relay) and the
   in-container glue that pipes socket <-> plugin stdio. Prove it with the EXISTING
   stub/roll machinery before any channel semantics.
2. Host channel relay implementing `channels.ChannelAdapter` over the boundary, reading
   `channel.inbound`, writing `channel.send`. Register it; make the router treat it
   like Telegram. Enforce identity namespacing in the relay (section 7).
3. Sub-shape 4a (inbound webhook) end to end, with the host relay owning the listener
   (resolution 1, section 6b). This validates the inbound and outbound paths with the
   worked example, no container egress needed.
4. Host-side `kind: channel` discovery and hot-reload (section 8).
   `channels.Registry.Unregister` is already in place for the hot-remove path.
5. Sub-shape 4b (outbound dialer) as the second worked example in goclawkit, validating
   that untrusted upstream-protocol parsing stays in the box.
6. Manifest: flip `internal/plugin/manifest.go`'s `case "channel"` from
   "not supported yet" to validated (a channel manifest has no `command`, lists its
   env var NAMES, declares `kind: channel`).

## 10. Open questions to resolve during build

- Container egress for 4b dialers: does the egress policy permit a chat-gateway dial
  from the container, or must specific upstreams be allowlisted (like the credential
  proxy's per-host injection)? Determines whether 4b option A (plugin dials) is viable.
- Does a pure inbound webhook deserve to be a plugin at all, or is it a first-party
  relay feature with the goclawkit `cmd/webhook` retained purely as the SDK's protocol
  demo? (Section 6b leans: first-party relay for production, plugin example for the
  SDK.)
- Socket mount path convention and podman flags for a read-write Unix-socket bind on
  rootless podman (SELinux `:Z` relabel interplay with a socket, not a dir).
