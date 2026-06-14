# Context, rules, and guardrails: principles for the agent layer

Status: DESIGN NOTES. Date: 2026-06-13. Four principles that should guide how goclaw
feeds context and rules to the in-container agent and how it gates the agent's work.
Some are already realized in the codebase (noted inline); some are sharpenings of the
current design or candidate future work. This doc is the "why", the per-area RFCs and
the code are the "how".

The throughline: **the agent should pull the context it needs, when it needs it, under
rules that are tiered and traceable, with deterministic and adversarial gates between
phases, and any file goclaw manages on the agent's behalf must update without clobbering
what a human edited.**

## 1. Broker context on demand; do not cram it into the system prompt

**Principle.** Context, rules, and reference material live where their owners maintain
them (a skill file, a vault note, the codebase itself, the event log) and the agent
*fetches* a topic when a task needs it. The base system prompt stays small and stable;
it routes to context, it does not carry all of it.

**Why.** A system prompt that grows to hold every rule, fact, and how-to has three
failure modes: it burns context budget on every single turn whether or not the turn
needs that material; it goes stale because nobody maintains a giant prompt the way they
maintain the doc the rule actually lives in; and a long, dense prompt is exactly where
models start ignoring individual rules. Pulling on demand keeps each turn's context
proportional to the task and keeps each rule maintained at its source.

**Where goclaw already does this.** The base prompt (`container/CLAUDE.md`) is
deliberately a small router plus a handful of always-true invariants. Skills load
themselves by `description` only when a task matches (coding always; librarian only with
a vault; introspection only with the event log mounted). The vault is pulled, not
inlined, the one always-on exception is `CRITICAL_FACTS.md`, which is small and L0 by
design. The "Finding things in a codebase" guidance already tells the agent to fetch
exact files / use a structured index rather than carry the layout in its head.

**The sharpening.** Treat growth of `container/CLAUDE.md` as a smell. Before adding a
paragraph to the base prompt, ask: can this be a skill the agent invokes, a vault note
it reads, or something it derives from the repo on demand? The base prompt earns a new
line only for something that must be true on *every* turn regardless of task (an
identity fact, a safety invariant, the mode router). Everything else is fetched.

```
Decision rule for "where does this rule/context go?"
  needed on EVERY turn, all modes      -> base prompt (container/CLAUDE.md)   [rare]
  needed only for a kind of task       -> a skill (loads by description)
  durable knowledge someone queries    -> the vault (pulled when relevant)
  facts about the running system        -> the event log (introspection skill reads it)
  layout / "what calls what"           -> fetch from the repo / structured index on demand
```

## 2. Rules should be tiered by severity and carry their provenance

**Principle.** Not all rules are equal, and the agent should be able to tell which is
which. Express the agent's operating rules in tiers, and where a rule comes from a
source the agent can cite, keep that citation.

### 2a. Tiered severity (must / should / may)

Today the base prompt's invariants are a flat bulleted list, with one prose sentence
bolted on to say which are non-overridable. A three-tier model makes the precedence
structural instead of narrative:

- **must (iron law).** Non-negotiable. Safety, honesty, and the containment boundary.
  The agent never breaks these, and a user instruction to break one is declined.
  Examples today: REPORT HONESTLY; the host/agent boundary is pull-only; do the
  deliverable, do not narrate it.
- **should (golden path).** The default way to work. The agent follows it unless the
  user explicitly asks otherwise, in which case it complies and says it is doing so.
  Examples: BE CONCISE; keep complexity low; tests with the code.
- **may (local convention).** Style and preference. Applied by default, freely
  overridden. Examples: a specific output format, a tone preference.

This maps cleanly onto the distinction the prompt already tries to draw ("safety rules
are not overridable; style rules are"). Tiering replaces the ad-hoc sentence with a
label on each rule, which is also what a per-group personality layer needs: a personality
may bend a `may` rule and even a `should`, but never a `must`. That is the open precedence
question in [`bot-personality.md`](./bot-personality.md), and severity tiers answer it.

Sketch of how the invariants section could read once tiered:

```markdown
## Invariants

### must (never broken, not even on request)
- REPORT HONESTLY. ...
- The host/agent boundary is pull-only; never open a write channel to the host. ...

### should (the default; user may override, you comply and say so)
- BE CONCISE. ...
- DON'T JUST AGREE. ...

### may (convention; override freely)
- ... style/format preferences, or a per-group personality file ...
```

A layered file (personality, per-group prompt) then states its precedence in one line:
"this file may adjust `may` and `should` behavior; it cannot override a `must`."

### 2b. Provenance / citation

A rule or fact the agent acts on should be traceable to where it came from, so a human
(or the agent, later) can audit why it did what it did. goclaw already enforces this for
*knowledge*: the librarian skill requires vault facts to carry provenance, and the
introspection skill ranks the event log (operational fact) above vault notes
(interpretation) above beliefs precisely so a claim can be traced to ground truth.

The gap is the agent's own *operating rules*: an invariant in the base prompt is just an
assertion with no pointer to the decision behind it. This matters less than knowledge
provenance (the rules are few and curated), so it is a low-priority sharpening, not a
must. If a rule's "why" is non-obvious, give it a one-line pointer to the doc or incident
that produced it, the same way a good code comment cites the bug it fixes:

```markdown
- must: never make the host a second writer of outbound.db.
  (why: SQLite over the podman mount corrupts under two writers; see the brief §5.1)
```

## 3. Gate phases with sensors: deterministic checks and adversarial review

**Principle.** Between "wrote the change" and "done" there is a gate, and the gate is two
kinds of check that both must pass:

- **Computational (deterministic).** A script with an exit code: format, vet, build,
  test, lint. The verdict is mechanical; there is no judgment and no drift.
- **Inferential (judged).** An LLM reviewing the change for the things a script cannot
  see: correctness reasoning, security, design. The verdict is an opinion, so it is
  advisory or majority-voted, not a hard exit code.

The value of naming them as one "phase gate" is that it makes the contract explicit: work
does not advance from implementation to done until the gate passes, and the gate is not
satisfied by either half alone. A green build with no review misses logic bugs; a glowing
review with red tests is worthless.

**Where goclaw already has both halves.** The deterministic half is the pre-commit hook:
gofmt, `go vet`, `go build ./...`, `go test ./...`, plus the stray-binary check, with the
race suite and golangci-lint in CI. The hook is a true gate, a tree that does not compile
or whose tests fail cannot be committed. The inferential half is the adversarial review
pattern (a reviewer prompted to find bugs / refute a finding) plus the project rule that
tests must have teeth (mutate the code, confirm the test fails). What goclaw does *not*
do today is frame these as one named gate the agent must clear in order; they are two
separate habits. That is fine, this is a model to keep in mind, not a thing to build,
but the framing is useful: when adding a new class of check, decide which kind it is
(does an exit code settle it, or does it need judgment?) and wire it into the same gate
rather than inventing a parallel path.

```
phase gate (implement -> done)
  computational  gofmt · go vet · go build ./... · go test ./...   (hard: exit code)
                 + golangci-lint, -race          (CI)
  inferential    adversarial code/security review                  (advisory/voted)
  advance only when BOTH are satisfied
```

A caution that matches goclaw's "fail closed" rule: a missing or skipped check is not a
pass. If a sensor cannot run, the gate is not green, it is unknown, and unknown is
treated as not-passed.

## 4. Manage files as an overlay: forward updates, never clobber user edits

**Principle.** When goclaw writes a file that a human is also expected to edit (a prompt,
a vault contract file, a per-group config), updates from a new goclaw version must refresh
only the parts goclaw owns and must never overwrite what the human changed. The unit of
update is "the goclaw-managed content", not "the whole file".

**Why.** The alternative, regenerating the whole file on upgrade, forces a choice between
losing the user's edits or never shipping prompt/contract improvements. An overlay model
gets both: goclaw pushes its updates forward, the human's content survives.

**Where goclaw already does this.** `goclaw vault sync` (`internal/vaultinit`) refreshes
the goclaw-OWNED operating-contract files in a live vault from the embedded template after
an upgrade. The mechanism is the load-bearing part:

- It works off an explicit allowlist (`ownedFiles`), not "diff the whole tree". Only files
  goclaw authored are candidates; user notes are never even considered.
- Before overwriting an owned file that differs, it writes a `<file>.bak` next to it, so a
  sync is non-destructive and reversible.
- `--dry-run` reports what *would* change (Added / Updated / Same) without touching disk.

```
goclaw vault sync  (sketch of the contract)
  for each rel in ownedFiles:            # explicit allowlist of goclaw-authored files
      embedded := template[rel]
      if !exists(rel):        Added      # create missing owned file
      elif equal(disk, embedded): Same   # already current, skip
      else:                   Updated    # back up disk -> rel.bak, then overwrite
  # anything NOT in ownedFiles is never read, never written, never backed up
```

**Where this principle should carry forward.** The per-group personality layer in
[`bot-personality.md`](./bot-personality.md) is exactly this shape: a personality file is
operator-owned content, and the composed base prompt is goclaw-owned content. Composition
must layer them so a goclaw upgrade refreshing the base never disturbs the operator's
personality file, and (if personality is ever scaffolded from a template) updates to that
template follow the same allowlist + backup + dry-run discipline as `vault sync`. The same
applies to any future "managed block in CLAUDE.md/AGENTS.md" idea: rewrite only the
goclaw-managed block, preserve everything around it.

## How these fit together

The four principles are one stance on the agent layer:

1. keep the always-on surface (the base prompt) small and **fetch** the rest;
2. when a rule must be on that surface, give it a **tier** so precedence is structural,
   and a **source** when its "why" is non-obvious;
3. gate work on a **deterministic + adversarial** pair that both must pass, and treat a
   skipped check as not-passed;
4. when goclaw manages a file a human also edits, **overlay**: forward-update the owned
   parts, back up before overwriting, never clobber user content.

None of these requires a large build; goclaw already realizes most of them. The value is
naming them so future changes are weighed against them: before growing the base prompt,
adding a flat rule, inventing a parallel check path, or regenerating a managed file,
check it against the relevant principle here.
