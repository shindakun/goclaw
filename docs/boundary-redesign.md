# Host <-> container boundary: a redesign exploration

Status: DECIDED. This document asked whether the host<->container message boundary,
currently "one SQLite pair per conversation over the podman mount", should move to a socket
now that a socket boundary also exists (the channel relay). **The decision (section 6/9):
KEEP the file boundary on the agent path, for security reasons, and freeze it.** The
analysis and the rejected socket option are retained below.

The framing constraint the author was given: **goclaw does not have to look like any
other system.** The two-DB design was inherited as a port of NanoClaw, and "NanoClaw does
it this way" is not by itself a reason to keep it. But after weighing it, the file boundary
is kept on its own merits (containment), not by inheritance. The deciding constraint that
emerged: the agent is untrusted, and the file boundary is the one that gives it NO live
channel to the host ("no escape, file or network").

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

### 3.1 The tax is not theoretical: a production incident (2026-06-04)

While this RFC was being written, the tax bit for real. The delivery loop logged:

```text
ERROR drain session  session=telegram:6306189728
      err="pragma \"PRAGMA journal_mode = DELETE;\": database is locked (5) (SQLITE_BUSY)"
```

Two HOST goroutines (the router enqueuing inbound, the delivery loop opening the same
session to write the ledger) raced to open `inbound.db`, and the open failed because
`journal_mode = DELETE` (a header write) ran before `busy_timeout` was in effect. The
proximate cause was a pragma-ordering bug, now fixed (busy_timeout first, single
connection first; see `internal/db/db.go` and its contention test). But step back: the
ENTIRE failure mode, `journal_mode = DELETE` taking a write lock and a busy_timeout
needed to wait it out, exists ONLY because we run SQLite across the mount with the
rollback journal. None of it would exist on a socket boundary. There is no
`journal_mode` to set, no header lock to contend, no `SQLITE_BUSY`, on a stream. This is
a concrete instance of the §3 "tax": real complexity, a real production error, and a
real fix, all in service of making SQLite survive the boundary rather than in service of
delivering a message. It is data for the recommendation in §6, not a one-off curiosity.

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
- **Security (the deciding axis, see section 6).** The agent container is untrusted
  (prompt injection; "tell the agent to find its way out of the box"). The files give a
  property no socket can: a **pull model with NO live channel from the box to the host**.
  The container writes a file in its OWN mount; the host reads it read-only on the host's
  schedule. The contained process cannot initiate anything toward the host, cannot connect
  out, cannot send a frame, cannot probe. Its entire reach across the boundary is "write
  bytes into a file in my own filesystem." A socket inverts this: the runner DIALS the
  host and holds an open, bidirectional, authenticated pipe from inside the box to the host
  process, a live channel a compromised/escaped agent can SPEAK on. Framed, capped, and
  token-gated though it is, it is a strictly larger attack surface than a one-way file
  write, and it is added to the MOST sensitive path (the core agent), not a narrow plugin.
  The earlier draft called the host-local ledger under B1/C "arguably a security
  improvement"; that is half the picture and the less important half. Yes, the durable
  store becomes host-local (good), but the agent GAINS a live outbound channel to the host
  (bad, and it is the half that matters for "no escape"). The host-can-treat-frames-as-
  hostile mitigation is real but it is mitigation of a surface the file model never opens.
  Net: on the agent path, files are the more contained choice. The channel-plugin socket
  already crosses this line, but channel plugins are narrow and explicitly untrusted; the
  agent runner is the thing we most want air-gapped from the host.

## 6. Recommendation

**KEEP Option A: the per-conversation SQLite file pair stays the agent message boundary.
This is a deliberate SECURITY choice, not "doing nothing." The socket stays where it
belongs, the channel plugins, and does NOT move onto the core agent path.** (An earlier
draft of this RFC recommended the opposite, moving to a socket, on complexity grounds.
That recommendation is reversed: it optimized the wrong variable.)

The reasoning, in priority order:

- **Security is the top constraint, and the file boundary wins it.** The decisive
  property (section 5) is that the file pair is PULL-ONLY: the untrusted, prompt-injectable
  agent has NO live channel to the host. It writes a file in its own mount; the host reads
  it read-only. The contained process cannot connect out, cannot send the host a frame,
  cannot probe, its entire reach is "write bytes into my own filesystem." A socket would
  hand the box a live, open, bidirectional pipe to the host process on the single most
  sensitive path. That is the exact "find its way out, file or network" capability we do
  not want to grant. No complexity saving is worth enlarging the agent's reach toward the
  host. This alone settles it.
- **"Two mechanisms" is correct defense-in-depth, not inelegance to eliminate.** The two
  boundaries sit at two trust levels, and matching the mechanism to the level is the right
  design: the agent runner (the thing we most want air-gapped) gets the most-isolated
  boundary (pull-from-files, no outbound channel); the narrowly-scoped channel plugins get
  the convenient socket. Collapsing them onto one socket "for elegance" would drag the
  agent path UP to the channel path's surface, the wrong direction. Uniformity is not a
  goal; containment is.
- **The complexity the socket would delete is real but already PAID and bounded.** The
  `mode=ro` enforcement, `journal_mode=DELETE`, open-write-close-per-op, and the
  corruption-streak respawn exist to make SQLite survive virtiofs, and yes they are a tax
  (and yes §3.1 was a real `SQLITE_BUSY`). But that machinery is written, tested, and
  working; the ongoing cost is low. Switching does not recover much, it TRADES this known,
  bounded set of subtleties for a new correctness-critical surface (a reliable-delivery
  ack/redeliver/dedup protocol) on the one path where a bug loses or doubles a user's
  message. That is a bad trade even before the security argument.

What we SHOULD do instead of a redesign:

- **Stop drifting.** The legitimate worry that prompted this RFC, "we keep adding special
  cases to the SQLite path", is addressed by NOT adding more, not by switching transports.
  The current set of cross-mount rules is the complete set; new features should not extend
  it. If a feature seems to need a new SQLite-over-mount special case, that is the signal
  to reconsider the feature, not the boundary.
- **Keep the framing honest** (done): the brief and CLAUDE.md now say "one pair per
  conversation, single-writer-per-file" and acknowledge the channel socket coexists, so
  no one mistakes "two mechanisms" for a defect to be refactored away.

The socket-based agent boundary (Options B1/C) is documented below and in section 7 for
completeness and so a future reader sees it was considered and consciously rejected, NOT as
a target. If goclaw's threat model ever changes (e.g. the agent runner becomes trusted, or
the durability/complexity pain genuinely exceeds the containment value), revisit, but that
is not today, and the burden is on the change to show it does not enlarge the box's reach.

## 7. A concrete staged path (kept for the REJECTED socket option, reference only)

The following is the migration that a socket-based agent boundary WOULD take. It is
retained so the rejected option is fully specified (and so a future revisit, see section 6,
does not start from scratch), NOT as a plan of record. The recommendation is to do none of
this and keep the file boundary.

The shape it would take:

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

## 8. What a socket would have COST us (i.e. what the file boundary keeps)

These are the properties the rejected socket option would lose, which is to say, the
reasons beyond the headline security argument that the file boundary is worth keeping:

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

## 9. Decision (made)

**DECIDED: keep Option A (the per-conversation file boundary) for the agent path, and
FREEZE it, add no new SQLite-over-mount special cases.** The socket stays scoped to channel
plugins. This is a security-first decision: the file boundary is pull-only (no live channel
from the untrusted box to the host), and that containment property outweighs the
complexity a socket would delete. The two-mechanisms shape is correct defense-in-depth by
trust level, not inelegance to refactor away (section 6).

What this commits us to:

- **No agent-path socket.** The runner never dials the host; the host never opens a live
  pipe into the box for messages. The contained agent's only cross-boundary reach stays
  "write a file in its own mount."
- **Freeze, do not extend, the cross-mount SQLite rules.** The current set (`mode=ro`,
  `journal_mode=DELETE`, open-write-close-per-op, corruption-streak respawn) is complete.
  A feature that seems to need a NEW special case on this path is a signal to reconsider the
  feature, not the boundary.
- **The framing is fixed** in the brief and CLAUDE.md (one pair per conversation,
  single-writer-per-file, the channel socket coexists) so "two mechanisms" is not later
  mistaken for a defect.

Revisit only if the threat model changes (the agent runner becomes trusted, or the
cross-mount pain genuinely exceeds the containment value). The burden is on any such change
to prove it does NOT give the box a live channel to the host. Until then, this is settled.
