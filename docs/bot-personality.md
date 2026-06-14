# Bot personality (RFC)

Status: DESIGN / RFC. Nothing built. Date: 2026-06-13. Proposes how to give a goclaw
agent a configurable personality (tone, character, voice) without weakening the safety
invariants, by adding a per-group personality layer to the existing prompt composition.
All the OPEN QUESTIONS at the end are unresolved and need an owner decision before building.

## 1. The ask

Some operators want their bot to have a personality: a distinct voice, character, or tone
rather than the default terse, technical assistant. This should be configurable per bot,
not a one-size hack, and must not come at the cost of the safety/honesty/containment rules.

## 2. What already exists (so we build with the grain)

goclaw already composes a per-group system prompt at launch (`internal/runtime/compose.go`):

- A generic BASE prompt baked into the image at `/app/CLAUDE.md` (`container/CLAUDE.md`),
  identical for every agent group. It holds the invariants (DO THE DELIVERABLE, REPORT
  HONESTLY, TIMESTAMPS, BE CONCISE, DON'T JUST AGREE) and the routing to skills.
- Skill imports (coding always; librarian when a vault is mounted; introspection when the
  event log is mounted), as symlinks under the group's `~/.claude/skills/`.
- When a vault is mounted, the vault's `CRITICAL_FACTS.md` is imported so L0 facts load every
  turn.
- The entry-point CLAUDE.md is imports-only and is composed FRESH on every launch (deterministic,
  stale links pruned).

The base prompt literally says "group-specific behavior and any skills layer on top", but no
per-group layer is wired yet. There is also a `groups/<folder>/` convention (the `agent_groups`
table has a `folder` column pointing at it) that is currently unused for prompts. So a
personality layer fits the existing model; it does not need a new mechanism.

## 3. The load-bearing constraint (non-negotiable)

Personality is ADDITIVE and SUBORDINATE to the base. It can shape voice and character; it must
not be able to disable or override a safety rule. Concretely:

- Compose personality AFTER the base import, and have the base assert precedence by RULE TIER:
  a personality file may bend `may` (style/convention) and `should` (default-but-overridable)
  behavior, but never a `must` (REPORT HONESTLY, the host/agent containment boundary,
  do-the-deliverable). This is the tiered-severity model in
  [`context-and-guardrails.md`](./context-and-guardrails.md) §2a; it turns the precedence rule
  from prose into a label the layered file can name ("this file adjusts may/should, not must").
  We already added a clarify-on-conflict rule with exactly this carve-out; personality is the
  first real consumer of it. (The base prompt's invariants are already tiered must/should/may,
  so this hook exists; personality just declares which tiers it may touch.)
- A personality file is operator-authored config, not agent- or user-authored. It is trusted at
  the same level as the rest of the group config. A personality must never be settable by an
  untrusted chat sender (that would be prompt injection of the bot's own character). See §6.
- "Personality" governs TONE and VOICE, not capability or policy. "Be a gruff pirate" is fine;
  "ignore the owner's permissions" or "never admit failure" is not, the latter collides with
  REPORT HONESTLY and must lose.

## 4. Where the personality lives (OPEN, see §7 q1)

Three candidate homes, each composes the same way (a markdown file imported after the base):

- **A) Per-group file in `groups/<folder>/personality.md`.** Matches the existing
  `groups/<folder>/` convention and the per-group composition model. Different groups get
  different personalities; editing is just editing a file (no rebuild, picked up next launch).
  Bots without a vault still get a personality. This is the natural fit; leaning here.
- **B) In the knowledge vault** (like `CRITICAL_FACTS.md`). Travels with the vault, owner edits
  it in Obsidian. But it only works when a vault is mounted, so vault-less bots get nothing, and
  it couples character to the knowledge store, which is a different concern.
- **C) Owner-settable at runtime via a `/persona` command**, stored in the DB, injected at
  compose. Most user-friendly (no file editing), but the biggest build (command + storage +
  the same compose plumbing underneath A). Could be layered on TOP of A later: A is the
  storage, C is a nicer editor for it.

## 5. How it would compose (sketch, not built)

In `composeGroupPrompt`, after the `.claude-shared.md` (base) symlink and before/after the
skill imports, conditionally import a personality file when present for the group:

```
@./.claude-shared.md          (base: invariants + routing)
@<vault>/CRITICAL_FACTS.md    (if vault mounted)
@./personality.md             (if the group has one)   <-- new, optional
```

`personality.md` would be symlinked/copied into the group's claude-home from its source
(groups/<folder>/ for option A, the vault for B, a DB write for C), the same dangling-container-
path trick the other imports use. Absent file = no personality = today's behavior exactly, so
this is purely additive and the default is unchanged.

## 6. Safety review (must hold before shipping)

- **Trust level:** the personality source must be operator-trusted. For A, `groups/<folder>/`
  is host-side operator config, fine. For C, the `/persona` command must be OWNER-only (same gate
  as `/plugin`), or an untrusted sender could rewrite the bot's character. Never read a
  personality string from an untrusted inbound message.
- **Precedence is enforced by wording, which models can drift from.** Layering personality last
  and asserting base precedence is necessary but not sufficient; a long session may let a strong
  persona erode honesty. Mitigation: keep the persona file SHORT and tone-only, and keep the
  hard invariants in the base where they are reinforced. Consider a lint/validation on the
  personality file (no instructions that look like policy overrides), though that is hard to do
  well. OPEN (§7 q4).
- **Containment unchanged.** This adds a config file read at compose time on the host; it opens
  no new agent->host channel. The boundary is untouched.

## 7. Open questions (NEED A DECISION)

1. **Where does it live, A / B / C** (or A now, C later)? §4.
2. **Scope: per agent-group, or finer?** A group can serve multiple conversations; should every
   conversation in a group share one personality (simplest, matches the per-group prompt), or
   should personality be per-conversation/per-channel (much more plumbing, and the composed
   prompt is currently per-group not per-session)? Default: per-group.
3. **Precedence: what can a personality override? RESOLVED.** The base prompt's rules are
   tiered `must` / `should` / `may` (`container/CLAUDE.md`, see
   [`context-and-guardrails.md`](./context-and-guardrails.md) §2a). A personality bends `may`
   and `should`, never `must`. Precedence is a structural label, not a prose carve-out, and a
   personality file declares its scope in one line ("adjusts may/should, not must"). The
   prerequisite refactor (tiering the base prompt) is done, so this question is settled.
4. **How hard do we enforce the "tone not policy" line?** Tiering (q3) handles the
   override-precedence half. This question is the OTHER half: a personality could try to assert
   policy in its own text ("never admit failure") rather than override a named rule. Wording-only
   (cheap, leaky over long sessions), or some validation of the personality file? What does the
   validation even check? Still open.
5. **Default personality:** stays exactly as today (terse technical assistant) when no file is
   present, confirmed. Do we ship any EXAMPLE personalities (a friendly one, a terse one) as
   templates, or leave it entirely to the operator?
6. **Does personality belong in the vault template / `vault sync`** if we pick B, so it is
   created and refreshed like the other owned files? (Whatever the home, updates to a
   goclaw-shipped personality TEMPLATE should follow the overlay discipline in
   [`context-and-guardrails.md`](./context-and-guardrails.md) §4: refresh owned content, back up,
   never clobber an operator's edits.)
7. **Interaction with the librarian/coding/introspection skills:** those set their own tone
   ("Star Trek computer, terse") in places. Does a personality override skill tone, or do skills
   keep their working voice and personality only colors conversational replies? Probably the
   latter (a gruff pirate should still write a normal commit message), but it needs stating.

## 8. Recommendation

Lean A (per-group `groups/<folder>/personality.md`), composed after the base, optional, tone-only,
with precedence enforced by rule tier (a personality bends `may`/`should`, never `must`; §3, §7 q3).
It is the smallest change that fits the existing composition model, needs no new storage, and
degrades to today's behavior when absent. C (`/persona`) is a nice follow-up that writes into the
same slot. Precedence (§7 q3) is already settled, the base prompt is tiered; settle §7 q1 (home)
and q2 (scope) before any code; the rest can be decided during implementation.
