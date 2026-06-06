# Local-network control: Sonos as the forcing case

Status: DESIGN / RFC. No code. This explores letting the agent control devices on the
operator's LAN (Sonos speakers: play, pause, volume, queue, group), and uses Sonos to force
a decision goclaw has so far avoided: **how, and whether, the sandboxed agent is allowed to
reach the local network.** Read `docs/security.md` (threat model) and
`docs/nanoclaw-go-podman-brief.md` (the boundary) first. The networking decision in section
3 is the substance; Sonos is just the concrete case that makes it unavoidable.

## 1. Why this is not "just another plugin"

Every integration so far (Telegram, Discord, IRC, Gmail) reaches OUTBOUND to the internet,
through the credential proxy when a credential is involved. That direction is the entire
shape of the security model: the container is NAT'd, egress goes out (optionally via the
TLS-intercepting proxy), and the host holds the secrets. The container never needs to see
the host's own network neighbors.

Sonos inverts this. There is no cloud API in the useful path: control is **on the LAN**,
speaker to controller, over local HTTP. Specifically:

- **Discovery is SSDP** (`M-SEARCH` over UDP **multicast** to `239.255.255.250:1900`), or
  mDNS. The controller multicasts "who is a Sonos here", speakers answer with their
  `http://<lan-ip>:1400/...` device-description URL.
- **Control is UPnP/SOAP** (and newer local HTTP/websocket APIs): HTTP POST to
  `http://192.168.x.y:1400/MediaRenderer/AVTransport/Control` with a SOAP body to play,
  pause, set volume, manage the queue, group/ungroup players.
- **Events are GENA** (the speaker calls BACK to a subscriber URL on the LAN), if you want
  push state instead of polling.

So a Sonos plugin needs to (a) DISCOVER peers on the host LAN and (b) make HTTP requests to
arbitrary `192.168.x.y:1400` hosts. Both are things the current container deliberately
cannot do. That is the whole point of this doc: Sonos cannot be added without first deciding
how local-network reach is granted, and that decision widens the threat model.

## 2. What the container can reach today (and why Sonos fails)

Rootless podman (the goclaw default) gives the container a slirp4netns/pasta user-mode
network: the container has its own NAT'd stack, reaches the internet outbound, and reaches
the host via `host.docker.internal`. It does NOT get an address on the operator's LAN, and
it cannot receive or send link-local **multicast** to the LAN segment. Concretely, in the
container today:

- `M-SEARCH` multicast to `239.255.255.250:1900` does not reach the LAN, so **SSDP
  discovery finds nothing**.
- Even with a hardcoded speaker IP, a POST to `http://192.168.1.50:1400/...` does not route
  to the host's LAN neighbor (the user-mode stack NATs to the host, not onto the LAN).
- `buildArgs` (`internal/runtime/runtime.go`) adds NO `--network` flag, so this is the
  default, by design: the container is contained.

So a Sonos plugin dropped in as-is would simply discover nothing and reach nothing. Making
it work means changing the container's network posture, which is a security decision, not a
plugin detail.

## 3. THE decision: how does the agent reach the LAN? (four options, with trade-offs)

This is the core of the RFC. Each option grants LAN reach differently, with a different
blast radius. They are ordered from most-contained to least.

### 3a. Host-side Sonos broker (RECOMMENDED). The container never touches the LAN.

Mirror the credential-proxy pattern: the HOST does the local-network work, the container
only talks to the host. A small `sonos` broker runs in the goclaw HOST process (which IS on
the LAN): it does SSDP discovery, holds the discovered device list, and exposes a TINY,
FIXED control surface (e.g. "list rooms", "play/pause room R", "set volume room R to N",
"group R into G"). The plugin in the container calls that surface over the EXISTING outbound
seam (`host.docker.internal`, or a channel-plugin-style endpoint), never the speakers
directly.

- **Blast radius:** the container gains the ability to issue a CONSTRAINED set of Sonos
  commands, NOT arbitrary LAN access. A compromised agent can annoy you with music; it
  cannot port-scan your LAN, hit your router admin page, or reach other local services.
- **Cost:** goclaw grows a real component (SSDP + UPnP/SOAP client) and an allowlisted
  command API. This is more host code, but it keeps the invariant that the container is
  contained and the host mediates anything sensitive, the same reasoning that put OAuth
  refresh in credstore, not the proxy.
- **Fits the existing shapes:** discovery/state is the host's; the command surface is small
  and auditable; the container stays NAT'd. This is the credential-proxy philosophy applied
  to the LAN: the dangerous capability (LAN reach) lives host-side, the container gets a
  narrow, mediated view.

### 3b. Proxy-style LAN allowlist. The container reaches ONLY enumerated speaker IPs.

Extend the bundled proxy (or a sibling) to forward to a SMALL ALLOWLIST of LAN hosts:port
(the known speaker IPs on :1400), and nothing else. Discovery still has to happen somewhere
(host-side, since multicast can't cross), and the operator (or a host-side discovery step)
configures the speaker IPs into the allowlist. The container's HTTP to `192.168.1.50:1400`
is permitted; everything else on the LAN is refused.

- **Blast radius:** narrower than full LAN, wider than 3a: the agent can send ANY HTTP
  (any SOAP action, any path) to the allowlisted speaker IPs, but cannot reach other LAN
  hosts. A compromised agent is confined to the speakers, but has the speakers' FULL local
  API, not a curated command set.
- **Cost:** allowlist plumbing + a host-side discovery step to populate it. Re-uses the
  proxy mental model (host decides what egress is allowed).
- **Caveat:** GENA event callbacks (speaker -> subscriber) do not work (the speaker cannot
  call back into the NAT'd container), so this is poll-only. Same limitation as 3a unless
  the host hosts the callback.

### 3c. Macvlan / LAN-attached container. The container gets its OWN LAN IP.

Put the container on a macvlan (or equivalent) network so it has a real address on the
operator's LAN and can multicast SSDP and reach any LAN host directly, like any other
device.

- **Blast radius:** LARGE and a real departure from the model. The agent is now a
  first-class device on your home/office LAN: it can discover and reach the router, NAS,
  printers, other people's laptops, IoT, everything. A container escape or a confused-deputy
  prompt injection now has your whole LAN as its reachable surface. This is exactly the
  containment the rest of goclaw works to prevent.
- **Cost:** low code (a podman network flag), HIGH security cost. Only defensible on a
  dedicated/isolated VLAN with nothing else on it.
- **Verdict:** do NOT do this for a general install. Note it exists, and that it is the
  wrong default precisely because it dissolves the boundary.

### 3d. Host network namespace (`--network host`). Strictly worse than 3c.

`--network host` gives the container the host's network stack outright (LAN + localhost +
every host-bound service). Mentioned only to reject it: it is 3c plus access to everything
listening on the host's loopback (including goclaw's own DBs/proxy). Never for an untrusted
agent.

### Recommendation

**3a (host-side broker)** is the right default and the one consistent with every prior
decision: the host holds the dangerous capability (LAN reach + discovery), the container
gets a narrow, allowlisted command surface, the boundary stays intact. **3b** is a
reasonable middle if we want the plugin to speak Sonos's protocol directly but stay confined
to speakers. **3c/3d are documented to be refused** for a normal install.

## 4. If 3a: what the host broker looks like (sketch, not a spec)

- **Discovery:** host-side SSDP `M-SEARCH`, cache the `{room/zone name -> lan ip}` map,
  refresh periodically (Sonos topology changes when players group/ungroup). Discovery is a
  CAPABILITY, gated by config (off by default; the operator opts in, like mounts).
- **Command surface (allowlisted verbs, not raw SOAP):** `rooms()`, `nowPlaying(room)`,
  `play(room)`, `pause(room)`, `next/prev(room)`, `setVolume(room, 0..100)`,
  `group(rooms...)`, `ungroup(room)`, maybe `playFavorite(room, name)` /
  `enqueueUri(room, uri)`. Deliberately NOT "POST arbitrary SOAP", so a prompt-injected
  agent's reach is the verb list, not the speaker's whole API.
- **Delivery to the container:** reuse an existing seam. Either a tool-plugin that calls the
  host broker over the channel-plugin endpoint pattern, or a small host-mediated tool. The
  plugin holds no LAN access; it asks the host to act, exactly like the credential proxy
  holds the token.
- **Authorization:** Sonos commands are owner-gated (and per-room allowlistable). "Set the
  volume to 100 at 3am" is annoying, not catastrophic, but it is still an action on the
  physical world, so it should be gated like any side-effecting capability, and rate-limited.

## 5. Threat-model delta (what we are signing up for)

Adding LAN control, even via 3a, widens the model in ways security.md must record:

- **New capability class: physical-world / local-network effects.** Until now a compromised
  agent could leak data or send messages. Now it can act on devices in your home (sound).
  Low severity for speakers, but it is the first "reach into the physical space" capability,
  and it sets precedent (lights, locks, thermostats would follow the same broker pattern and
  are NOT all low-severity, a door lock is not a speaker). The doc should say plainly: this
  pattern generalizes, and the broker's verb allowlist is the control that keeps each device
  class's blast radius bounded. Do not let "it's just music" justify a generic "control the
  LAN" capability.
- **Discovery is information disclosure.** SSDP enumerates devices on the LAN; even host-side
  discovery means the agent learns what's on your network (room names, device types). Keep
  discovery results host-side and expose only what a command needs (room names), not the raw
  device inventory.
- **Default off, opt-in, like mounts.** LAN control is not on unless the operator enables it
  and (ideally) names the rooms/devices in scope. Fail closed: no config, no LAN reach.

## 6. Open questions (decide before building)

1. **3a vs 3b:** host broker with a curated verb set (safest, most host code), or proxy
   allowlist letting the plugin speak SOAP to speaker IPs only (less host code, wider
   per-speaker reach)? Leaning 3a for the precedent it sets for higher-severity devices.
2. **Where the broker lives:** in the goclaw host process, or a separate host-side helper
   (like the credential proxy is bundled but separable)? Bundled is simpler; separable keeps
   goclaw core smaller.
3. **Discovery cadence and caching:** how stale can the room map be? Sonos grouping changes
   often; a wrong cache means commanding the wrong zone.
4. **Events:** poll-only (simplest, works with any option) vs host-hosted GENA callbacks for
   push state. Poll-only for v1.
5. **Does this become a generic "local device broker"?** If lights/locks follow, the broker
   should be designed as a capability framework with a per-device-class verb allowlist and
   per-class severity, NOT a Sonos special case. Worth deciding the shape now even if only
   Sonos is built, so the second device isn't a rewrite.

## 7. Relationship to the OAuth/second-provider question

This doc exists because "add a second OAuth provider (e.g. Spotify) to prove the engine" ran
into the fact that Spotify cannot make sound come out (its Web API only controls an existing
Spotify Connect device; the agent is in a container with no audio). Sonos is the real
"play music I can hear" path, but it is a LOCAL-NETWORK problem, not an OAuth one: it needs
no cloud token, it needs LAN reach. So the two are orthogonal: the OAuth engine is proven by
a request-level cloud provider (Microsoft/Spotify) if we want; Sonos is proven by the
local-network broker decided here. Do not conflate them.
