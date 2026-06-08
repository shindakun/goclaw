# Host event log + agent introspection (RFC)

Status: DESIGN / RFC. Nothing built. Date: 2026-06-07. Proposes a single host-owned,
append-only operational event log and an agent skill that reads it for self-diagnosis. The
goal: let the agent (and the operator) answer "what did the system actually DO, and why did
that go wrong" from one ground-truth artifact, instead of reconstructing it from scattered
logs after the fact. Several bugs this codebase has already hit (a scheduled task that fired
during an outage and was silently lost; the credential-proxy CA drifting and flooding
bad-cert errors) are exactly the class this would surface.

## 0. The containment constraint (non-negotiable, shapes the whole design)

The agent is untrusted (prompt injection; the box must not be escapable, file or network).
So this design is **host-writes, agent-reads-only**, and never the reverse:

- The HOST owns the event log and is the only writer. The agent NEVER writes to it (no
  loopback "emit an event" API into the host, that would be a write channel from the box to
  the host, exactly the surface we refuse to add; see the boundary decision in the brief
  §5.1).
- The agent gets a READ-ONLY view, via the existing read-only mount pattern (the plugins
  dir is already mounted `:ro` into the container; the event log rides the same mechanism).
- Nothing here changes the message boundary (the per-conversation SQLite pair stays). This
  is a new, additive, read-only artifact, not a new control channel.

## 1. The gap

The host ALREADY emits structured events, it just throws them away as unstructured log
lines. Today `slog` carries "scheduled task fired", "maintenance job fired", "credential
proxy active", "channel plugin attached", "deferring message for retry (transient failure)",
and dozens more. They go to the host's stdout/journal, are not queryable, are not retained
in a stable schema, and the AGENT cannot read them at all. So:

- The operator reconstructs an incident from `podman logs` + host stdout + guesswork (we did
  exactly this for the CA drift and the IRC ban this week).
- The agent has NO way to inspect what the system did. It can read the vault and its own
  conversation history, but not "did my 07:00 task actually run, and if not, why."

There are three adjacent, DIFFERENT logs already, and the event log must not be confused
with them:

- **The vault `log.md`** is a KNOWLEDGE audit (the librarian's append-only record of vault
  mutations). It is about notes, not operations. It stays the vault's, not this.
- **`.install-log.jsonl`** (added for plugin provenance) is a narrow, append-only JSONL of
  install/remove events. It is, in effect, the FIRST slice of the event log proposed here;
  this RFC generalizes that exact pattern.
- **The SQLite session pair** is the message boundary, not a log.

So the event log is a NEW thing: a unified, host-owned, append-only, agent-readable record
of OPERATIONAL events (what the host and runner did), distinct from knowledge (vault) and
from the message transport (SQLite).

## 2. What it is

A single append-only JSON-lines file the host writes, one event per line, stable schema:

```json
{"ts":"2026-06-07T07:00:03-07:00","kind":"schedule.fired","agent_group":1,"task":"inbox","ok":true}
{"ts":"2026-06-07T07:00:03-07:00","kind":"schedule.deferred","agent_group":1,"task":"inbox","reason":"ensure runner: outage"}
{"ts":"2026-06-07T13:11:11-07:00","kind":"proxy.ca_generated","detail":"new CA identity minted; running containers trust a stale cert"}
{"ts":"2026-06-07T13:11:11-07:00","kind":"delivery.failed","channel":"telegram","chat":"...","reason":"..."}
```

- **Common fields:** `ts` (RFC3339 local), `kind` (dotted namespace, e.g. `schedule.*`,
  `delivery.*`, `proxy.*`, `plugin.*`, `runner.*`, `channel.*`), and `ok` where a
  success/failure split is meaningful. Everything else is kind-specific.
- **Append-only, rotated by size/age** (a long-running host must not grow it unbounded). A
  simple `event-log.jsonl` + `event-log.1.jsonl` rotation, no external dependency.
- **One writer.** It is host-process-local (single writer trivially), so it does not inherit
  any of the cross-mount SQLite hazards, it is a plain host file the host appends to.

## 3. Where it lives and how the agent reads it

- **On the host:** under the data dir, e.g. `data/events/event-log.jsonl`. Host-owned,
  host-written.
- **In the container:** mounted READ-ONLY at a fixed path, e.g. `/run/goclaw/events/` (the
  same `:ro` mount mechanism the plugins dir uses). The agent reads it; it physically cannot
  write it (the mount is read-only AND the host is the only writer).
- **Per-agent-group scoping (open question, section 7).** An event log may contain events
  about OTHER agent groups / conversations. An agent should probably see only events for its
  own group (and only operational events, never another user's message content). The
  simplest safe default: write a PER-AGENT-GROUP log (`data/events/<agentGroupID>/...`) and
  mount only that group's into that group's container. Cross-group/system events that are
  not sensitive can go in a shared `system` log also mounted read-only.

## 4. What gets logged (and, importantly, what does NOT)

Log OPERATIONAL facts, never secrets or raw user content:

DO log: schedule fired/deferred/skipped; maintenance job ran; a turn was answered / deferred
for retry (the transient-failure path); delivery sent/failed/denied; a plugin
installed/removed/updated; the runner launched/relaunched/was reaped; the proxy generated a
new CA / refreshed a token (the EVENT, not the token); a channel attached/detached; an error
with its category.

Do NOT log: credential values or tokens (only "refreshed credential for host X"); raw
message bodies or vault note contents (only metadata, "delivered message id N to chat C");
anything that, mounted read-only into the box, would leak a secret INTO the container. The
log is read by an untrusted agent, so it must be safe for the agent to read in full. This is
a real review burden per event kind, and a reason to keep the kinds curated, not "log
everything."

## 5. The introspection skill (reads the log, does not write the system)

A vault/agent skill (same SKILL.md mechanism as the librarian) that teaches the agent to
diagnose itself from the log. Its disciplines, the genuinely useful part:

- **Source-of-truth hierarchy.** The event log is GROUND TRUTH for what the system DID
  (operational fact). The agent's own memory/beliefs may be stale; when they conflict with
  the event log about an operational fact, the log wins. (Note the deliberate split from the
  knowledge side: the VAULT is ground truth for KNOWLEDGE; the event log is ground truth for
  OPERATIONS. Two truths, two domains, do not conflate them, that is the same auto-memory-
  vs-vault distinction that already bit us.)
- **THAT-not-WHERE / fix-the-system.** When the agent finds something went wrong, it does
  not just patch the symptom; it asks why the SYSTEM produced that outcome and proposes a
  structural change. The output of a root-cause pass MUST be a verifiable artifact, a file
  edit, a config/schedule change, a vault note, NOT a resolution to "remember to do better."
  A behavioral fix that produces no diff did not happen.
- **Self-audit as a scheduled job.** A maintenance-style job (we already have the
  maintenance scheduler) periodically reads recent events, looks for failures/drift (e.g.
  "a scheduled task fired but its session shows no answered turn", "deliveries to channel X
  are failing repeatedly"), and surfaces a short report to the owner, with the structural
  fix it took or recommends.

Crucially, the skill READS the event log and WRITES to the vault / its own outputs / the
owner chat. It never writes the event log, and it has no new ability to act on the host. Its
power is diagnostic, not a new control surface.

## 6. Why this is worth it (and the honest cost)

Worth it:

- It is the missing observability layer. Every incident this week was diagnosed by hand from
  ephemeral container logs; a retained, structured, queryable log makes that a query, and
  lets the AGENT do the first pass.
- It turns the maintenance scheduler we already have into a self-correction loop, not just a
  vault-upkeep loop.
- It is boundary-safe by construction (host-writes, agent-reads-only, additive), so it does
  not weaken containment.

Honest cost:

- **Per-kind redaction review.** Every event kind must be vetted to never carry a secret or
  raw user content, because an untrusted agent reads it. This is ongoing discipline, not a
  one-time check.
- **Rotation/retention.** A long-running host needs the log bounded; trivial but must be
  built, not forgotten.
- **Yet another log.** We now have host slog, container logs, vault `log.md`,
  `.install-log.jsonl`, and this. The event log should SUBSUME `.install-log.jsonl` (fold
  plugin events into it as `plugin.*` kinds) so we do not accumulate parallel JSONL files,
  and the docs must be clear which log is which (knowledge=vault, operations=event log,
  transport=SQLite).

## 7. Open questions

1. **Per-group vs system scoping.** Per-agent-group logs (mounted only into that group) is
   the safe default, but cross-group/system events (CA generated, host started) need a home.
   A shared read-only `system` log seems right; confirm nothing in it is group-sensitive.
2. **Retention policy.** Size cap, age cap, how many rotated files. Default: a few MB, a few
   files, deleted oldest-first. Decide concrete numbers.
3. **Does `.install-log.jsonl` fold in now or later?** It is the same pattern; folding it in
   avoids a parallel log but is a small migration (move the install events into the event
   log's `plugin.*` kinds). Probably do it when the event log lands.
4. **Schema discipline.** A registry of event kinds (so `kind` strings do not drift) vs
   free-form. Leaning a small typed set in one place, like the existing `installEvent`.
5. **How aggressively does the self-audit job act?** Report-only to the owner, or allowed to
   make low-risk structural fixes (e.g. re-enable a wrongly-paused task) on its own? Default
   report-only; acting autonomously on the host is the thing to gate carefully.

## 8. Phasing

1. **Host event log core:** a small `internal/eventlog` package (one writer, append JSONL,
   rotation), a typed event-kind set, and call sites at the existing slog points (schedule
   fire/defer, delivery, proxy CA, plugin install/remove, runner lifecycle). Read-only mount
   into the container. Fold in `.install-log.jsonl`.
2. **Introspection skill:** the SKILL.md teaching the source-of-truth hierarchy + the
   read-the-log disciplines, shipped in the templates (and reachable via `vault sync`).
3. **Self-audit job:** a scheduled maintenance job that reads recent events, flags
   failures/drift, and reports to the owner. Report-only first.
4. **(Later, if wanted) structural-fix authority:** let the audit job make narrowly-scoped,
   reversible fixes, gated and logged as its own events.

## 9. Recommendation

Build phase 1 (the host event log) and phase 2 (the introspection skill) together: the log
without a reader is just more retention, and the skill without the log has nothing to read.
Defer the self-audit job (phase 3) until the log has accrued real events to audit. Keep the
whole thing host-writes / agent-reads-only and additive; if any part of it ever needs the
agent to WRITE the log or act on the host through it, stop, that crosses the containment
line this design exists to respect.
