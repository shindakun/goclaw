# Host event log + agent introspection (RFC)

Status: PARTIALLY SHIPPED. Date: 2026-06-07 (updated 2026-06-12). Proposes a single host-owned,
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
- **`.install-log.jsonl`** (added for plugin provenance) WAS a narrow, append-only JSONL of
  install/remove events. It has since been FOLDED INTO the event log (kinds `plugin.install`
  / `plugin.remove`); it was the first slice of this pattern and no longer exists as a
  separate file.
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

### 2.1 ONE log, many kinds (do not split the operational log)

A deliberate design choice: every operational event goes into the SAME file, discriminated by
`kind`. We do NOT keep a separate `schedule.jsonl`, `delivery.jsonl`, `proxy.jsonl`. The
separation between event categories is the `kind` field, queried with a filter, not the
filesystem. Reasons:

- **Cross-cutting questions need one stream.** "What happened around 07:00 when the task
  failed" spans `schedule.*`, `delivery.*`, and `proxy.*` at once. On one timestamped stream
  that is a single filtered read; across three files it is a manual timestamp-merge. Collision
  signatures (two firings in the same second, a duplicate delivery) are ONLY visible when the
  categories share a stream.
- **One writer, one rotation, one schema, one redaction review.** N logs is N of each, and N
  chances for them to drift out of sync. The install log was already folded in for exactly
  this reason (section 1); the same logic says do not split it back apart by category.
- **`kind` is the cheap discriminator.** Looking at only the scheduler is
  `select(.kind | startswith("schedule."))`, not a different file. You get category isolation
  without paying for parallel logs.

The split that DOES belong on the filesystem is across NATURE, not category: operational FACT
(this log) vs the agent's KNOWLEDGE/interpretation (the vault) vs mutable STATE (the
`scheduled_tasks` DB rows, not an append log) vs message TRANSPORT (the SQLite session pair).
Those are genuinely different things with different writers and trust levels (section 1). The
rule is: split across nature, never within the fact log.

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

- **Source-of-truth hierarchy (explicit, ordered).** Different sources answer different
  questions and rank differently when they CONFLICT about an operational fact. From most to
  least authoritative on "what actually happened":

  1. **The event log** , operational FACT (what the host/runner did). Ground truth for
     operations.
  2. **The channel / `outbound.db`** , what was actually SENT/received (external truth,
     re-readable). Beats the agent's recollection of a conversation.
  3. **Vault notes** , the agent's own INTERPRETATION/knowledge. Useful for intent, but it is
     narrative, not operational fact.
  4. **Auto-memory / beliefs** , the agent's current model of the world. May be stale; lowest.

  When they disagree about an operation, trust higher over lower: event log > channel history >
  vault > beliefs. This is the same auto-memory-vs-vault distinction that already bit us, made
  into a ranked rule. Keep the two ground truths apart: the VAULT is ground truth for
  KNOWLEDGE, the event log is ground truth for OPERATIONS. Do not conflate them.

- **Query recipes, not just prose.** The skill's most useful half is concrete: the event
  schema plus ready `kind`-filtered queries the agent runs to slice the one log (last N events;
  errors in the last session; all `schedule.*`; events grouped/counted by `kind`; two events in
  the same second across sessions = a collision signature). Teaching the agent the schema and a
  handful of these filters is most of the skill's value, the diagnosis is a query, not a hunt.
- **THAT-not-WHERE / recommend-the-fix.** When the agent finds something went wrong, it does
  not just note the symptom; it asks why the SYSTEM produced that outcome and says how the system
  should change. Crucially, the agent is ADVISORY: it is sandboxed and cannot reach the host, so
  it does not apply the fix itself. The output of a root-cause pass MUST be a concrete artifact,
  an owner message with the recommended change, a vault note, or (for a genuine code bug, with a
  token) a PR on a BRANCH against the goclaw repo, NOT a resolution to "remember to do better"
  and NOT a claim to have changed something it cannot touch. A pass that produces no such
  artifact did not happen. (The shipped skill spells out exactly which outputs are possible.)
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
- **One log feeds two readers.** The same file the agent introspects can render a read-only
  OPERATOR view (a `goclaw events`/dashboard tail) for free, the human and the agent diagnose
  from the identical ground truth instead of two divergent stories. A cheap byproduct of
  keeping it one structured stream, not a separate build.
- It is boundary-safe by construction (host-writes, agent-reads-only, additive), so it does
  not weaken containment.

Honest cost:

- **Per-kind redaction review.** Every event kind must be vetted to never carry a secret or
  raw user content, because an untrusted agent reads it. This is ongoing discipline, not a
  one-time check.
- **Rotation/retention.** A long-running host needs the log bounded; trivial but must be
  built, not forgotten.
- **Yet another log.** We have host slog, container logs, vault `log.md`, and this. The event
  log SUBSUMED `.install-log.jsonl` (plugin install/remove are now `plugin.*` kinds), so there
  is no parallel JSONL; the docs stay clear about which log is which (knowledge=vault,
  operations=event log, transport=SQLite).

## 7. Open questions

1. **Per-group vs system scoping. RESOLVED (interim).** Shipped as a single shared log mounted
   read-only ONLY when exactly one agent group exists (`db.CountAgentGroups() == 1`); with more
   than one group the host refuses the mount and logs why, so no group ever reads another's
   events. This is fail-closed and unblocks the single-group deployment today. The fuller answer
   (per-agent-group logs plus a shared non-sensitive `system` log) is still the eventual design
   when multi-group support arrives; the gate is the placeholder until then.
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

1. **Host event log core: SHIPPED.** `internal/eventlog` exists (one writer, append JSONL,
   size+age rotation, a typed event-kind set) with call sites at schedule fire/defer, delivery
   sent/denied/failed, proxy CA, plugin install/remove (the install log was folded in), and
   runner lifecycle (`runner.launched` on an actual container (re)launch in `internal/runtime`,
   `runner.reaped` on idle GC in `internal/sweep`). The read-only container mount is shipped
   too: `data/events/` is mounted `:ro` at `/run/goclaw/events/`, GATED FAIL-CLOSED on there
   being a single agent group (the one shared log can carry other groups' events; with >1 group
   the host logs and does NOT mount). That gate is the chosen answer to open question 1 for now:
   single shared log, mounted only when one group exists, until a per-group log is built.
2. **Introspection skill: SHIPPED.** `container/skills/introspection/SKILL.md` teaches the
   source-of-truth hierarchy + the read-the-log disciplines. It is BAKED INTO THE IMAGE
   alongside `coding` (it needs only the mount, not a vault), and `compose.go` links it into a
   group's `~/.claude/skills/` only when the event log is mounted (mirroring how `librarian` is
   gated on a vault).
3. **Self-audit job:** a scheduled maintenance job that reads recent events, flags
   failures/drift, and reports to the owner. Report-only first. STILL DEFERRED (per §9): let the
   log accrue real events first.
4. **(Later, if wanted) structural-fix authority:** let the audit job make narrowly-scoped,
   reversible fixes, gated and logged as its own events.

## 9. Recommendation

Build phase 1 (the host event log) and phase 2 (the introspection skill) together: the log
without a reader is just more retention, and the skill without the log has nothing to read.
Defer the self-audit job (phase 3) until the log has accrued real events to audit. Keep the
whole thing host-writes / agent-reads-only and additive; if any part of it ever needs the
agent to WRITE the log or act on the host through it, stop, that crosses the containment
line this design exists to respect.

## 10. Example introspection SKILL.md (draft)

The draft below was phase 2's design; it SHIPPED (with `runner.*` kinds added) as
`container/skills/introspection/SKILL.md`, baked into the image and gated in `compose.go` on the
event-log mount. It is the operational counterpart to the librarian skill (knowledge) and is
loaded the same way (a `SKILL.md` the CLI auto-invokes by `description`). Its prerequisite, the
read-only event-log mount at `/run/goclaw/events/`, also shipped (phase 1). The shipped file may
have drifted slightly from this draft; treat the file as canonical. The host file is
`data/events/event-log.jsonl`.

````markdown
---
name: introspection
description: Diagnose goclaw's own behavior from the operational event log at /run/goclaw/events/. Use when something operational went wrong or looks off: a scheduled task that did not run, deliveries failing or being denied, the proxy CA churning, a plugin install/remove you need to confirm, or auditing your own recent operations. Read-only: this skill READS the event log and writes its findings to the vault or the owner chat; it NEVER writes the event log or acts on the host. Do NOT use for knowledge work (use librarian) or one-off chat.
---

# Introspection

Your operations leave a trace. The host writes an append-only, structured event log of what
the SYSTEM actually did; you read it to diagnose problems and propose structural fixes. You
read it; you never write it (the host is the only writer, and the mount is read-only).

## Source-of-truth hierarchy (when sources disagree about an operation)

1. **The event log** (`/run/goclaw/events/event-log.jsonl`) - operational FACT. Ground truth
   for what the host/runner did.
2. **The channel / conversation** - what was actually SENT/received. Beats your recollection.
3. **Vault notes** - your own INTERPRETATION. Narrative, not operational fact.
4. **Your current beliefs** - may be stale. Lowest.

Trust higher over lower: event log > channel history > vault > beliefs. The VAULT is ground
truth for KNOWLEDGE; the event log is ground truth for OPERATIONS. Do not conflate them.

## The log: one file, one event per line, discriminated by `kind`

```json
{"ts":"2026-06-09T07:00:03-07:00","kind":"schedule.deferred","ok":false,"fields":{"task":"inbox","reason":"ensure runner: outage"}}
{"ts":"2026-06-09T07:05:03-07:00","kind":"schedule.fired","ok":true,"fields":{"task":"inbox","owner":7}}
{"ts":"2026-06-09T07:05:09-07:00","kind":"delivery.sent","ok":true,"fields":{"session":"telegram:42","channel":"telegram","chat":"42","msg_id":91}}
```

Kinds you will see: `schedule.fired` / `schedule.deferred`; `delivery.sent` / `delivery.denied`
/ `delivery.failed`; `proxy.ca_generated`; `plugin.install` / `plugin.remove`. Common fields:
`ts` (RFC3339 local), `kind`, `ok` (present where success/failure is meaningful), and
`fields` (kind-specific). There are no message bodies or secrets here by design; do not expect
them.

## How to query (diagnosis is a filter, not a hunt)

```bash
LOG=/run/goclaw/events/event-log.jsonl

# Last 20 events.
tail -n 20 "$LOG" | jq .

# Did my 07:00 task hand off, or defer? (the "fired but didn't complete" class)
jq -s 'map(select(.kind|startswith("schedule."))) | sort_by(.ts)' "$LOG"

# All failures/denials.
jq -c 'select(.ok == false)' "$LOG"

# Events by kind, counted (what is happening most).
jq -s 'group_by(.kind) | map({kind:.[0].kind, n:length}) | sort_by(-.n)' "$LOG"

# Deliveries to a channel that keep failing.
jq -c 'select(.kind=="delivery.failed") | .fields.channel' "$LOG" | sort | uniq -c

# Did the proxy CA churn (the bad-cert-flood signature)?
jq -c 'select(.kind=="proxy.ca_generated")' "$LOG"
```

## Find the WHAT, then fix the SYSTEM

When you find something wrong, do not just patch the symptom: ask why the SYSTEM produced it
and produce a VERIFIABLE artifact. Examples:

- `schedule.deferred` with no later `schedule.fired` for the same task => the task is stuck
  (the runner never came back). The fix is a config/schedule change or a note to the owner with
  the evidence, NOT "I will remember to check."
- Repeated `delivery.denied` for one target => an authorization gap; surface it with the exact
  channel/chat from `fields`, propose the destination rule that is missing.
- A `proxy.ca_generated` you did not expect => running containers now trust a stale CA;
  tell the owner to recreate runners (the documented remedy).

A root-cause pass that ends without a diff, a config change, a vault note, or a concrete owner
message did not happen. Write the finding to the vault (your interpretation) and, if it needs
the owner, say so plainly with the event evidence.

## Boundaries

You READ the event log and WRITE to the vault / the owner chat. You never write the event log
(read-only mount; the host is the sole writer) and you gain no new ability to act on the host.
Your power here is diagnostic, not a control surface.
````

Notes on the draft:

- It is deliberately SHORT on prose and LONG on schema + queries, the query recipes are the
  reusable value; the agent should be diagnosing with a `kind` filter, not reasoning from
  memory.
- It encodes the SAME source-of-truth ranking and the SAME "fact vs knowledge" split as the
  rest of this doc, so the introspection skill and the librarian skill never claim each
  other's ground.
- It is read-only by construction and says so, matching the containment line: the agent reads,
  the host writes, and the agent's outputs land in the vault or the owner chat, never back in
  the log.
- Ship it via the template + `vault sync` (like the librarian skill), OR baked alongside
  `coding` if it should be present with no vault, AFTER the read-only mount exists. Without the
  mount the skill has nothing to read.
