# Host <-> container boundary: a redesign exploration

Status: EXPLORATION / RFC. Nothing here is built. This document asks whether the
host<->container boundary, currently "two SQLite files per session over the podman
mount", is still the right shape now that a socket boundary also exists (the channel
relay), and lays out the design space honestly so we can decide rather than drift.

The framing constraint the author was given: **goclaw does not have to look like any
other system.** The two-DB design was inherited as a port of NanoClaw. That was a fine
starting point, but "NanoClaw does it this way" is not a reason to keep it. This doc
treats the boundary as an open design question.

## 1. What changed, and why it forces the question

The original premise (brief §5.1) was clean and quotable: *the host and the agent
container talk ONLY through two SQLite files per session, one writer each. No IPC, no
socket, no shared mutable state.* That single mechanism was sold as the thing that
"explains everything."

It is no longer true. Building channel plugins, we put a real **socket** (TCP, or a
Unix socket on native Linux) across the exact same host<->container line, because a
channel needs a long-lived low-latency duplex stream and (on macOS) a mounted Unix
socket cannot even be connected across the podman VM (`operation not supported`). So
today the boundary is BOTH:

- agent traffic: two SQLite files per session, over the virtiofs mount;
- channel traffic: a framed socket the runner dials and the host accepts.

Two mechanisms doing the same job (move bytes host<->container) is the smell. Your
question, restated precisely: *if we already have a working socket, do the SQLite files
still earn their place, or are they cargo we kept because the system we copied had them?*

## 2. What the boundary actually carries, and what each part NEEDS

Strip it to the cargo. Five things cross (or could cross) the boundary:

| Traffic | Direction | Needs durability? | Needs low latency? | Needs ordering? | Lives where now |
|---|---|---|---|---|---|
| Agent inbound (chat -> agent) | host -> runner | YES (survive a crash) | mild | per-session | `inbound.db` |
| Agent outbound (reply -> delivery) | runner -> host | YES (at-least-once) | mild | per-session | `outbound.db` |
| Delivery ledger (sent/not) | host-only | YES | no | no | `delivered` table in `inbound.db` |
| Channel in/out (IRC, ...) | both | NO (dropped chat = fine) | YES | mild | socket (built) |
| Control (shutdown, wakeup, heartbeat) | host -> runner | no | YES | no | partly env/files |

The key split the current design half-acknowledges: **agent traffic wants durability;
channel traffic wants a live stream.** They are not the same problem, and trying to make
one mechanism serve both is what produced the awkwardness.

## 3. Why the two-DB apparatus exists (the honest version)

It is easy to mythologize the SQLite pair. Strip it down. It buys exactly two things:

1. **Durability / at-least-once.** The files are a persistent queue. Runner crashes
   mid-reply -> the inbound row is still on disk -> host respawns the runner -> it
   resumes. The `delivered` ledger (idempotent `INSERT OR IGNORE` by message id) means a
   reply is sent at-least-once and de-duplicated. This is real and valuable for an agent.
2. **A boundary that survives the host or container restarting independently.** Either
   side can die and come back and pick up where it left off, because the state is on disk,
   not in a connection.

Everything else about it, `journal_mode=DELETE`, `mode=ro` on the host's outbound
handle, open-write-close per op, the corruption-streak-detect-and-respawn, the
single-writer rule, is not a FEATURE. It is the TAX you pay to run SQLite across a
virtiofs mount where page caches are not coherent. It is overhead in service of (1) and
(2), not a benefit in itself. A socket pays none of that tax because there is no shared
on-disk page cache to go incoherent.

So the design question sharpens to: **is the durability (1,2) worth the cross-mount-
SQLite tax, given we now have a socket that provides neither the durability nor the
tax?**

## 4. The option space

Four genuinely different shapes. For each: what it is, what it costs, what it kills.

### Option A: Status quo. Keep the SQLite pair for the agent, socket for channels.

Two boundary mechanisms, each where it fits. Agent = durable files; channel = live socket.

- PRO: it works today, fully built and tested. The durability story is intact. No
  migration risk.
- CON: two mechanisms, two mental models, two failure modes. The brief's "one mechanism
  explains everything" is dead; a new reader has to learn both. The cross-mount SQLite
  tax (all of §3's overhead) stays forever. The corruption-streak respawn is a real
  operational wart we keep.
- VERDICT: the safe default. Defensible, but it is the "we drifted and stopped" outcome.

### Option B: Everything on a socket. Delete the SQLite boundary.

The agent path moves onto the same framed socket the channel relay uses. Inbound is a
frame host->runner; outbound is a frame runner->host. No session DB pair. Durability is
provided by... what? You must answer that, or you lose at-least-once. Sub-options:

- B1: host-side durability only. The HOST keeps a queue/ledger (in the central
  `goclaw.db`, which the container never touches, so NO cross-mount SQLite at all). The
  host writes an inbound row, sends it over the socket, and only marks it consumed when
  the runner acks the frame. The runner holds nothing durable; if it dies, the host
  re-sends unacked inbound on reconnect. Outbound: the runner sends an outbound frame,
  the host writes it to its OWN db and acks; the runner can forget it once acked.
- B2: no durability. Accept that a crash mid-message loses it (like the channel path).
  Simplest, but a dropped USER message to the agent is much worse than a dropped IRC
  line; probably unacceptable.

- PRO (B1): ONE boundary mechanism for everything. The entire cross-mount-SQLite tax
  disappears, no `mode=ro` dance, no `journal_mode=DELETE`, no corruption-streak respawn,
  no two-writer hazard, because the container never touches a shared SQLite file at all.
  The durability moves to the host's own DB, which only the host writes (single-writer is
  now trivially true, it is one process). This is the design that actually delivers the
  "one mechanism" clarity the brief promised but the current split breaks.
- CON: a reconnect/ack protocol is real work and real risk. You are now implementing
  reliable messaging over a socket (acks, redelivery on reconnect, dedup) that SQLite-as-
  a-queue gave you for free. Subtle: exactly-once is impossible; you get at-least-once
  with dedup, same as now, but you own the state machine. The runner must tolerate a
  socket drop and re-handshake without losing its session affinity.
- VERDICT: the principled redesign. Most work, highest payoff in simplicity, real risk in
  the reconnect protocol. This is the "doesn't need to look like another system" answer.

### Option C: Socket for liveness, files for durability (hybrid, but inverted).

Keep SQLite, but make it HOST-OWNED ONLY and never cross the mount. The host writes both
the inbound queue and a delivery ledger to its own `goclaw.db` (no per-session files in
the container at all). The live host<->runner traffic goes over the socket. The files are
purely the host's durability log; the container reads/writes nothing on a shared mount.

- This is essentially B1 with the durability explicitly in host-local SQLite (which B1
  already implied). Calling it out separately because it reframes the SQLite from "the
  boundary" to "the host's write-ahead log," which is a much less scary role for it.
- PRO: durability is preserved with battle-tested SQLite, but with ZERO cross-mount
  hazard (the container never opens those files). Single-writer is trivially true.
- CON: same reconnect-protocol work as B1.
- VERDICT: B1 stated honestly. The recommended target (see §6).

### Option D: A different durable transport (not SQLite, not a bare socket).

E.g. a length-prefixed append-only log file the host owns, or an embedded queue, or
gRPC-with-a-WAL. Worth naming so it is consciously rejected: it trades a well-understood
tool (SQLite) for a bespoke one to solve a problem SQLite already solves. No.

## 5. The cross-cutting concerns any option must answer

A redesign that ignores these is hand-waving. Each option above has to satisfy:

- **Crash semantics.** Host dies: on restart, un-acked inbound must be re-presented and
  un-delivered outbound must be re-sent (deduped). Runner dies: its in-flight turn is
  lost from memory; the inbound that caused it must be redeliverable. Option A gets this
  from the files. Options B1/C must get it from the host ledger + ack protocol.
- **Multi-session affinity.** One container serves many sessions (one per chat). The
  boundary must keep messages routed to the right session. Files do this by directory.
  A socket does it by a session id in the frame, fine, but it is now explicit state.
- **Backpressure.** A slow agent must not let inbound pile unbounded. Files give natural
  backpressure (unconsumed rows sit on disk). A socket needs a window/credit or a bounded
  in-flight count.
- **The macOS VM reality.** Whatever we pick must work across the podman VM. SQLite-over-
  mount barely does (the whole tax). A socket does, but on macOS it must be TCP to
  host.docker.internal (the channel path already proved this); a mounted Unix socket does
  NOT work there. So a socket-based agent boundary inherits the channel path's transport
  choice and its per-connection auth token need.
- **Security.** The agent container is untrusted (prompt injection). Today the files give
  a clean story: the container can only write its own `outbound.db`; it cannot reach the
  host's central DB. A socket to the host is a NEW capability the untrusted container
  gets, the host must treat every frame as hostile input (it already does for channels:
  framed, capped, token-gated, identity not trusted). This is not a blocker (the channel
  socket already crosses this line) but it must be designed, not assumed. Notably: under
  B1/C the host's durable ledger is HOST-LOCAL and the container never touches it, which
  is arguably a SECURITY IMPROVEMENT over today, where the container writes a file the
  host then reads across a mount.

## 6. Recommendation

**Target Option C/B1: one socket boundary for live traffic, durability in a host-owned
log the container never touches. But do it as a deliberate, staged migration, not a
big-bang rewrite, and only if we judge the simplification worth the reconnect-protocol
work.**

The reasoning, plainly:

- The two-DB-over-mount design is the single largest source of incidental complexity in
  the system: the `mode=ro` enforcement, `journal_mode=DELETE`, open-write-close-per-op,
  and the corruption-streak-detect-and-respawn loop ALL exist solely to make SQLite
  survive virtiofs. None of it is intrinsic to "deliver a message to an agent." A socket
  deletes all of it.
- We ALREADY built and proved the hard part: a framed, authenticated, durable-enough
  socket across the podman VM, with reconnect tolerance (the relay re-accepts on a
  container bounce). The agent boundary would reuse that machinery, not invent it.
- Moving durability to a HOST-LOCAL db makes the single-writer rule trivially true (one
  process, one writer) and removes the untrusted container's write access to a
  cross-mount file. That is simpler AND safer.
- The honest cost is a reliable-delivery protocol over the socket (ack, redeliver-on-
  reconnect, dedup). That is real, but it is bounded and well-understood, and we already
  carry a dedup ledger today.

If we judge that cost too high for the payoff right now, **Option A is an acceptable
"do nothing" but we should stop pretending it is elegant**: it is two mechanisms, and the
brief should say so (it now does). What we should NOT do is keep drifting, adding the
third and fourth special case onto the SQLite path while the socket sits there proving
the alternative works.

## 7. A concrete staged path (if we pick C/B1)

So this is not just philosophy:

1. Define the agent frames on the existing protocol: `agent.inbound` (host->runner,
   carries session id + message), `agent.outbound` (runner->host, carries reply +
   correlation), `agent.ack` (both ways). Reuse the ipc framing, like channels did.
2. Host-side durable queue in `goclaw.db` (host-local, single-writer): an `inbox` table
   (pending agent-inbound per session) and the existing `delivered` ledger, both written
   ONLY by the host. No per-session DB files.
3. Runner attaches to the host over the same socket transport the channel relay uses
   (TCP+token on macOS, Unix on Linux), multiplexing all sessions for its agent group on
   one connection, keyed by session id in the frame.
4. Delivery: runner emits `agent.outbound`; host writes it to its own db, delivers via the
   channel adapter, marks delivered in the ledger, acks. Redeliver unacked on reconnect.
5. Inbound: host sends `agent.inbound` for pending rows; runner acks on consume; host
   marks consumed. Unacked survive a runner bounce and are re-sent on re-attach.
6. Delete `internal/db`'s session DB pair, the `mode=ro` outbound handle, the
   `journal_mode=DELETE` special-casing, and the runner's corruption-streak respawn. This
   is the payoff: a large net deletion.
7. Keep the per-session sessions DIRECTORY (the agent's conversation history,
   `~/.claude`, transcripts) exactly as is, that is the agent's own state, not the
   boundary, and it is mounted normally without the single-writer hazard.

Step 7 matters: this redesign is about the MESSAGE boundary, not about the agent's
on-disk conversation state, which legitimately stays a mounted directory.

## 8. What this would cost us that the current design has

Be honest about the losses, not just the wins:

- **Inspectability.** Today you can `sqlite3 outbound.db` and see exactly what the agent
  produced, post-mortem, with no host running. A socket-based boundary has no such
  artifact; you would lean on the host's log + the host db. The agent's conversation
  history (the sessions dir) is still inspectable, but the in-flight message boundary
  stops being a file you can open. This is a real debugging-affordance loss.
- **Total decoupling.** Today the host and runner share NO live connection; either can be
  absent and the other just sees stale files. A socket couples their liveness: the runner
  must handle the host being away (reconnect) and vice versa. We accept this already for
  channels, but it becomes true for the core agent path too.
- **A migration with correctness risk.** The delivery path is the one place a bug means a
  user's message is lost or doubled. Changing it is higher-stakes than the channel work
  was (a dropped IRC line is forgivable; a dropped or doubled agent reply is not). This
  argues for a careful staged rollout with the old path runnable side-by-side behind a
  flag during bring-up.

## 9. Decision needed

Three ways to go, pick one:

1. **Commit to C/B1** (socket boundary + host-local durability). Biggest simplification,
   real reconnect-protocol work, highest-stakes migration. The "build it right, not like
   the system we copied" answer.
2. **Stay on A** but own it: two mechanisms, documented as such, no pretense of one-
   mechanism elegance. Lowest risk, keeps the cross-mount tax forever.
3. **Defer**: leave A in place, but freeze the SQLite boundary (add no new special cases
   to it), and revisit C/B1 when the next thing that wants the boundary shows up. A
   conscious "not yet," not a drift.

The author's lean: **(1) is the right end state**, because the cross-mount SQLite tax is
pure incidental complexity that a socket deletes, and we have already de-risked the
socket. But (3) is a legitimate, honest interim if the appetite for a delivery-path
migration is not there right now. (2) is only acceptable if we genuinely will not revisit,
which seems unlikely given the trajectory.
