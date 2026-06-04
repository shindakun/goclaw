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

"A channel binds a port" hides a fork. It decides WHERE the network endpoint lives,
which in turn decides the goclaw-side deployment wiring (section 6 shows the webhook
plugin itself needs no code change either way). There are two sub-shapes:

### 4a. Inbound-listener channels (the webhook shape)

The channel is a SERVER: something outside POSTs to it. `cmd/webhook` binds
`WEBHOOK_ADDR` itself inside `Start()`. In the sandbox that bind SUCCEEDS (it binds a
port in the container's network namespace); the only open question is how the outside
world reaches it. Two ways, both decided host-side, neither touching the plugin code:

- The host PUBLISHES the container port (podman `-p`), so an external POST lands on the
  in-container listener directly and the plugin emits `channel.inbound` up the boundary.
  (Reachability A in section 6b. The worked example already does exactly this.)
- OR the host relay owns the public listener and the plugin binds nothing, so a pure
  inbound webhook needs no plugin process at all and becomes a first-party relay
  feature. (Reachability B; this is the variant that WOULD need a plugin without its
  own bind.)

The "does a pure inbound webhook even deserve to be a plugin" question lives here and is
resolved in section 6b: yes for the SDK demo, reachability-A; first-party relay for
production is the open call (section 10).

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
change." Short answer: **the webhook plugin needs NO code change, the SDK needs NO
wire change, and `-selftest` keeps working untouched.** The only open choice is a
goclaw-side DEPLOYMENT one (how the outside world reaches the in-container listener),
not an edit to `cmd/webhook`.

### 6a. The goclawkit SDK: no wire/protocol change needed

- The frame protocol (`pkg/ipc`) is unchanged: same four frame types, same header. A
  Unix-socket boundary is a new `Transport`, which `ipc` already supports by design.
- The channel topics (`channel.inbound`, `channel.send`, `channel.action`) are already
  defined and frozen in `serve_channel.go`. The relay speaks exactly these.
- `ServeChannel` is unchanged: it reads/writes frames over `ipc.StdioTransport` (the
  plugin's stdin/stdout). In the shim model the IN-CONTAINER RUNNER connects the
  plugin's stdio to the Unix socket; the plugin still just does stdio. So a channel
  plugin author writes the SAME `ServeChannel(ch)` they write today.

### 6b. The webhook plugin: no code change, just a reachability choice

It is tempting to say "the plugin binds its own port, so it cannot run in the sandbox."
That is WRONG, and worth being precise about. The webhook plugin binds `WEBHOOK_ADDR`
inside `Start()` (`main.go:113-118`, `listen(c.addr)`). When the plugin runs in the
agent container, that bind succeeds: it binds a port INSIDE the container's network
namespace. The only question is whether the OUTSIDE WORLD can reach that port, which is
a host deployment concern, NOT a property of the plugin code. The plugin's `Start()`
listener is therefore NOT dead code: it is exactly what receives the inbound POST. The
plugin compiles and runs as-is.

So the webhook plugin needs no change. goclaw has two ways to make the in-container
listener reachable; this is the deployment choice, decided host-side:

- REACHABILITY A (publish the container port): the host publishes the plugin's
  container port to a host address (podman `-p`), so an external POST hits the host and
  lands on the in-container listener directly. Inbound never crosses the framed
  boundary at all in this shape: it arrives over HTTP straight to the plugin, and the
  plugin emits `channel.inbound` up the boundary to the relay. Outbound (`channel.send`)
  still crosses the boundary. Simplest, and the worked `cmd/webhook` already does
  exactly this with no edits.
- REACHABILITY B (host relay owns the listener): the host relay binds the public HTTP
  listener and the plugin does not bind anything; an inbound POST is normalized by the
  relay and emitted directly. This removes the untrusted process from the inbound path
  entirely (a pure inbound webhook then needs no plugin process), but it means the
  PRODUCTION inbound-webhook is a first-party relay feature, with `cmd/webhook` retained
  as the SDK's protocol demo. It also requires the plugin to NOT bind a port, which the
  current example does, so it is the resolution that WOULD need a plugin variant.

Recommendation: ship REACHABILITY A for the worked example, because it needs zero edits
to `cmd/webhook` and proves the channel.* path end to end with the binary as-is. Keep B
in mind as the "do we even want a process here" question for production inbound webhooks
(section 10), but it is not required to run the example.

### 6c. `-selftest` is unaffected

`-selftest` (`selftest.go`) never calls `Start()` and never binds the real port: it
constructs the channel, calls `decodeInbound` directly, then `Send` to an in-process
`httptest` sink, and prints the round trip. It has NO dependency on where the plugin
runs or how the boundary is carried, so the shim changes nothing about it. It keeps
working verbatim (verified: `go run ./cmd/webhook -selftest` prints the inbound and the
delivered outbound and exits 0). Whatever we do host-side, `-selftest` stays the
no-host local smoke test it is today.

### 6d. The shape that justifies a channel PLUGIN: 4b (outbound dialer)

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
3. Sub-shape 4a (inbound webhook) end to end via reachability A (publish the container
   port; the plugin keeps its own listener, no edits; section 6b). This validates the
   inbound and outbound paths with the worked example as-is, no container egress needed.
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
