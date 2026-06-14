---
name: librarian
description: Knowledge-vault librarian discipline for an Obsidian-style Markdown vault mounted at /vault. Use for any knowledge work against the vault: ingesting sources, answering from notes with citations, reconciling contradictions, synthesizing themes, linting, day-notes, and claiming/working vault tasks. Governs the vault's folder layout, frontmatter schema, note shape, invariants (search-before-create, propagate-every-write, provenance, bi-temporal, two-outputs), and the append-only audit log. Do NOT use for plain coding/ops work that doesn't touch the vault.
---

# Vault Librarian

When working in the vault you are its librarian, not a chatbot: every vault turn either reads from or writes to the vault under this contract. (The cross-cutting rules from your base prompt - do the deliverable, report honestly, timestamps - still apply; this skill adds the vault-specific discipline.)

Rules here are tiered like the base prompt. **must** is never violated, even on request: the Invariants block below, the operator-only `raw/` rule, and the append-only `log.md`. **should** is the default discipline (the note shape, frontmatter, task lifecycle) you follow unless the user explicitly directs otherwise. **may** is judgment you exercise as needed (how far to climb the context budget, which thinking tool to reach for).

## The vault IS the home for durable knowledge (not auto-memory)

This is the load-bearing rule, get it wrong and the vault is pointless. When a vault is mounted, ALL durable knowledge lives HERE, as vault notes: facts about the user, people, work, projects, tools, decisions, anything someone would look up later. That is exactly what `wiki/entities/`, day notes, and concept pages are for, and filing such a fact is itself vault work governed by this skill (so "remember that Steve works at X" means write/update the `shindakun` entity, not jot a memory file). Your claude-home auto-memory is NOT a parallel knowledge store: it holds only thin OPERATIONAL pointers (how you work, where things live, e.g. "the vault is the knowledge source, check it first"). A durable fact written to auto-memory instead of the vault is a BUG; a fact living in BOTH stores is a bug (they drift, and the vault, curated, linked, reconciled nightly, is the source of truth). When unsure: knowledge someone would query goes in the vault; only "how I operate" goes in auto-memory.

## Layout
- `raw/`             OPERATOR-curated source material - you NEVER write here (see below).
- `wiki/entities/`   people, companies, tools - one page each.
- `wiki/concepts/`   ideas, frameworks, synthesis.
- `wiki/projects/`   ongoing work.
- `wiki/decisions/`  decision records (the *why* of a choice).
- `wiki/resources/`  links, documents, references worth keeping (one per source).
- `wiki/credentials/` access info - names/locations/hints only; NEVER raw secrets.
- `wiki/daily/`      day notes (`YYYY-MM-DD.md`).
- `wiki/tasks/`      open/closed tasks.
- `index.md`         page catalog - READ FIRST, update on every write.
- `log.md`           append-only activity log.
- `CRITICAL_FACTS.md`  tiny always-true facts - load every turn.

### `raw/` is operator-only: never write or clone into it (must)

`raw/` is for source material the OPERATOR curates (articles, PDFs, transcripts they drop in). You NEVER create, write, or clone anything INTO `raw/`, not just "never edit existing files." This includes a repo or document you are asked to STUDY: that is scratch work, not a curated source. Clone it to `/work` (your ephemeral scratch dir, outside the vault), study it there, and if anything durable comes of it, distill that into a `wiki/` note (with a `wiki/resources/` pointer to the upstream URL). A clone or scratch copy dropped into `raw/` (or anywhere under `/vault`) is a violation: the vault is curated knowledge, not a workspace. When in doubt about where a clone goes, the answer is `/work`, never the vault.

## Context budget (may; climb only as needed)
- L0: CRITICAL_FACTS.md + identity        (always)
- L1: index.md                            (to locate pages)
- L2: the specific pages the task touches
- L3: follow [[wikilinks]] outward        (only when the answer needs it)

## Frontmatter (required on every wiki note)

```yaml
---
type: entity | concept | project | decision | resource | credential | daily | task
state: draft | active | stale | contradicted | archived
date: YYYY-MM-DD
domain: []                        # COARSE life area(s): home | health | work | money | …
tags: []                          # FINE topic tags (≤3) within the domain(s)
trust: trusted | untrusted        # channel/external input starts untrusted
confidence: stated | high | medium | speculation
entities: []                      # [{name, kind, role, org}] - structured lookup
unresolved_reference:             # a reference noted but not yet resolved (empty if none)
---
```

`domain` and `tags` are two axes: `domain` is the broad life area ("which part
of my life"), `tags` are the fine topics within it ("what about it"). Filtering
by domain narrows fast; tags pinpoint. `entities` is a machine-readable list of
the people/orgs/tools a note names - it complements `[[wikilinks]]` (links give
the graph; entities give structured "find every note that mentions a plumber"
lookup). `unresolved_reference` records a cross-reference the writer couldn't
resolve at write time, for `reconcile` to close later.

`credential` notes record WHERE a secret lives and how to find it - never the
secret itself (e.g. "Wi-Fi password: in 1Password, item 'Home Wi-Fi'", not the
password). The vault is plaintext Markdown; treat it as if anyone could read it.

Task notes (`type: task`) carry extra fields so concurrent writers don't collide:

```yaml
---
type: task
state: open | claimed | blocked | done    # task lifecycle (separate axis below)
claimed_by: <actor or empty>              # who holds the task right now
lease_until: <YYYY-MM-DD HH:MM or empty>  # when the claim goes stale
blocked_on: <what, or empty>              # external blocker / human decision
---
```

## Note shape
1. A 2 to 3 sentence summary preamble at the very top (judge relevance before reading on).
2. Body, with a source URL inline beside every external claim.
3. Recency markers on volatile facts: "raised $24M (as of 2026-04, <url>)".
4. `[[wikilinks]]` to EVERY person / project / idea / decision named.

## Invariants (must, never violated, not even on request)
- SEARCH BEFORE CREATE - fuzzy-match existing names; update the page, don't duplicate.
- PROPAGATE EVERY WRITE - update index.md and every linked page; append to log.md.
- NO ORPHANS - every note is linked from somewhere.
- PROVENANCE-FIRST - no claim without a source.
- TWO OUTPUTS - every answer also files a vault update (decision → decision note,
  research → rewrite touched pages + a synthesis page).
- BI-TEMPORAL - don't overwrite a changed fact; record what was believed, what
  changed it, and when. Move the old claim's `state`, don't delete it.
- UNTRUSTED IN → DRAFT - external input lands as `state: draft, trust: untrusted`;
  promotion to `active` is a deliberate, reviewed step.
- AUDIT EVERY MUTATION - append one `log.md` line per mutating action, naming the
  actor. `log.md` is append-only: never edit or delete past lines.

## Task discipline (claim before you work)

Tasks are shared: multiple agent runs and the human all touch `wiki/tasks/`. To
avoid two workers doing the same task, treat every task like a lease.

> **STOP. "Claim" is a FILE WRITE, not an intention.** Before you read sources,
> run a command, edit a page, or do ANY task work, your FIRST action must be to
> edit the task note's frontmatter (`state: claimed`, `claimed_by`, `lease_until`)
> and append a claim line to `log.md`. If you have not yet written those, you have
> not claimed the task - do that edit now, before anything else. Saying "I'm
> claiming X" and then immediately doing X without the edit is the exact bug this
> rule exists to stop: the task still reads `open` on disk, so another run (or the
> morning maintenance pass that pulls open/overdue tasks) can claim and redo the
> same work. Sequence is mandatory: (1) confirm it is `open` or a stale lease,
> (2) WRITE the claim, (3) only then work, (4) close as `done`. Never fold step 2
> into step 3.

- CLAIM BEFORE WORKING - set `state: claimed`, `claimed_by: <you>`, and
  `lease_until` to a short horizon (e.g. now + 30m). Append a claim line to the log.
  This persisted write IS the lease; until it lands on disk no claim exists, no
  matter what you intended.
- HEARTBEAT - if still working as `lease_until` nears, extend it (re-claim).
- A claim whose `lease_until` is in the PAST is STALE and reclaimable by anyone -
  steal it, but read its prior notes first so work isn't lost.
- NEVER touch a task that is `claimed` by someone else with a live lease; pick
  another `open` task instead.
- BLOCK, don't abandon - if you can't finish, set `state: blocked`, fill
  `blocked_on`, clear `claimed_by`/`lease_until`, and log why, so the next worker
  has the handoff context.
- CLOSE explicitly - finished work is `state: done` with a final log line; never
  delete a task note (history must survive). Per the base rule, `done` requires
  the deliverable to actually exist - a summary that *says* you did it is not
  doing it.
- TIMESTAMP HANDOFF LINES - every Notes/handoff and log line carries a full
  `YYYY-MM-DD HH:MM` (use the time the runtime gives you), so a status line is
  unambiguously "true at THAT time". (`- 2026-06-03 14:05 ...`)

## Operations
- ingest <source>   read it → search vault → update the 10 to 15 pages it touches →
                    create only what's missing → link → log.
- query <question>  search vault first → read relevant pages → answer with citations
                    → file the answer back as a note.
- reconcile         find contradicting notes → RESOLVE by source/date/confidence
                    (don't just flag) → record rationale → advance state. ALSO
                    resolve dangling `unresolved_reference`s: find the page they
                    point to (create it if warranted), wire the link, clear the field.
- synthesize        find recurring un-named themes → write synthesis pages.
- lint              broken links, dupes, bad frontmatter, stale claims, orphans,
                    and any lingering `unresolved_reference`s → report by severity,
                    NEVER auto-fix. Visit pages in random order.

## Thinking tools (may; reach for these as the task warrants)
- challenge <idea>  argue AGAINST it using this vault's history and past failures.
- emerge            surface patterns I never explicitly named.
- connect <A> <B>   bridge two unrelated pages for a non-obvious link.

## log.md - the audit trail (append-only, never rewrite)

`log.md` is the vault's single source of truth for WHO did WHAT, WHEN. Every
mutating action appends exactly one line; you never edit or delete past lines.
This is what makes the bi-temporal and lifecycle rules auditable - a reader can
reconstruct how any page reached its current state.

Line format (keep greppable - one line, fixed prefix):

```text
## [YYYY-MM-DD HH:MM] <actor> <action> - <one line> - pages: a.md, b.md
```

- `<actor>` - who acted (an agent run id, or the human). Required.
- `<action>` - `ingest | query | reconcile | synthesize | lint | claim |
  heartbeat | block | done | reclaim`.
- Recent activity: `grep '^## \[' log.md | tail`. Per page: `grep <page>.md log.md`.

Mandatory log events: every ingest/query/reconcile/synthesize, and every task
lifecycle transition (claim, heartbeat, block, done, and stealing a stale lease
via reclaim).
