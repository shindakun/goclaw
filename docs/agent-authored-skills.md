# Agent-authored skills (RFC)

Status: DESIGN / RFC. Nothing built. Date: 2026-06-07. Proposes letting the agent capture a
repeated workflow as a new reusable skill (a `SKILL.md`) that the system then auto-discovers
on later turns, so the agent improves its own capabilities over time within the existing
sandbox. Works with OR without a vault (section 3). Read `internal/runtime/compose.go` (skill
composition) and the librarian skill template first.

## 0. Containment constraint (shapes the design)

The agent is untrusted; nothing here may give it a new channel out of the box or a way to run
code the host did not vet. This proposal is safe because it adds NO new surface: the agent
writes a markdown file into a directory it ALREADY has write access to (the vault), and the
existing skill-composition picks it up. A skill is PROMPT text the agent reads, not code the
host executes. Capability that needs host execution stays where it already is, the plugin
system (operator-gated, built in a throwaway container, scanned). Skills are instructions to
the agent; plugins are tools the agent calls. This RFC is only about the former.

## 1. What we have, and the one gap

goclaw already loads skills the standard way: `composeGroupPrompt` symlinks skill dirs into
the container's `~/.claude/skills/`, and the CLI auto-invokes a skill when a task matches its
`description`. Two skills ship today (`internal/runtime/compose.go`):

- `coding` , baked into the image at `/app/skills/coding`, ALWAYS linked, vault or no vault.
- `librarian` , provided by the vault at `/vault/.claude/skills/librarian`, linked ONLY when
  a vault is mounted (`if vaultMounted`).

Two host-writable roots already exist that an authored skill could live under, and the
distinction matters for the no-vault case (section 3):

- `claude-home` , the per-group host dir at `<data>/claude-home/<id>`, created and mounted
  read-write at the container's `~/.claude` (`/home/agent/.claude`) on EVERY launch,
  independent of any vault (`session.go`). This is where the composed `CLAUDE.md` and the
  skill symlinks already get written.
- the `vault` , mounted read-write at `/vault` only when configured.

So the agent CAN already write a `SKILL.md` into a host-mounted dir today, with or without a
vault. **The gap is discovery:** `composeGroupPrompt` hardcodes the exact two skills it links
(`coding`, and `librarian` when a vault is present); a newly-authored skill is never
symlinked in and the CLI never sees it. So agent-authored skills are two small pieces away:
(a) scan a skills root and link ALL skills found, not just the two hardcoded names; (b) a
discipline that tells the agent when and how to author one well.

## 2. Why this is worth it

- **Real self-improvement, bounded.** An agent that notices it has done the same multi-step
  workflow several times (a particular research-then-file sequence, a recurring ops recipe)
  can capture it as a skill, so next time it is one auto-invoked unit instead of re-derived
  from scratch. This is the difference between an agent that starts cold on a recurring task
  and one that accretes competence.
- **It fits the grain.** Skills are already markdown + frontmatter, already vault-resident
  (librarian), already composed at launch. This extends a mechanism we have rather than
  adding one.
- **It is durable and inspectable.** A vault skill is a git-tracked file the operator can
  read, edit, or delete in Obsidian, same as any vault note. Nothing hidden.

## 3. Where agent-authored skills live (vault preferred, but NOT vault-only)

A skill an agent authors should live in ONE of two host-writable roots, chosen by whether a
vault is configured. This is the correction to the naive "put it in the vault" answer: a
no-vault deployment is a first-class config (everything vault-related in compose is gated
`if vaultMounted`), and those agents must STILL be able to accrete skills, exactly as they
already keep the always-linked `coding` skill.

- **Vault present (preferred):** `/vault/.claude/skills/<name>/SKILL.md`, alongside
  `librarian`. The vault is the agent's read-write, git-tracked, durable, operator-visible
  store, so an authored skill is exactly that kind of artifact: a captured, reusable piece of
  knowledge-about-how-to-work. It survives container bounces, shows up in Obsidian, and rides
  the existing vault mount.
- **No vault (fallback):** `claude-home/skills/authored/<name>/SKILL.md`. `claude-home` is
  the per-group host dir already created and mounted read-write at `~/.claude` on every
  launch, vault or not, so a skill written there persists across container bounces with no new
  mount. It is NOT git-tracked and not surfaced in Obsidian (that is what choosing a vault
  buys), but it keeps self-improvement available to the no-vault config instead of silently
  disabling it.

In BOTH cases the discovery mechanism is the same (section 4a): compose scans the relevant
skills root and links every skill dir found. The only difference is which root.

NOT in `/app/skills/` (baked into the image, read-only, ships with goclaw). Image skills are
goclaw's; authored skills are the agent's/operator's. This mirrors the ownership split we
already use for the librarian skill and the `vault sync` command (goclaw owns the contract
files; the agent/operator own the rest).

## 4. The two pieces

### 4a. Composition: scan a skills root and link ALL skills (the enabling change)

`composeGroupPrompt` should, in addition to the always-linked `coding`, SCAN the active
authored-skills root for `*/SKILL.md` and symlink EVERY such subdirectory into
`~/.claude/skills/`, not just the hardcoded `librarian`. Which root is scanned depends on the
vault (section 3):

- **Vault mounted:** scan the vault skills dir (the HOST path to `/vault/.claude/skills/`,
  since compose runs host-side) for `*/SKILL.md`, adding one symlink per skill dir whose
  target is the CONTAINER path `/vault/.claude/skills/<name>` (dangles on the host, resolves
  in the container, exactly as the librarian link does today). `librarian` is just one of the
  results now, no longer the only one linked.
- **No vault:** scan `claude-home/skills/authored/` (a real host path that compose can read
  directly, no dangling-link trick needed since it is under claude-home itself) for
  `*/SKILL.md`, linking each into `~/.claude/skills/`.

In both cases:

- The existing prune logic still removes links for skills that no longer exist, so deleting
  an authored skill (or unmounting the vault) cleans up its link.
- `coding` (image skill) is unchanged and always linked.
- A name collision (an authored skill named `librarian` or `coding`) must NOT shadow the
  goclaw-owned one: composition treats `coding` and `librarian` as RESERVED and never lets a
  scanned skill of that name override them.

### 4b. A skill-creator discipline (when/how to author one well)

A short skill (or a section in an existing one) teaching the agent:

- **When to author a skill** , a workflow done 2-3+ times, with a repeatable shape, where
  capturing it saves re-derivation. NOT for one-off tasks (that is just doing the task).
- **The trigger-description bar** , the frontmatter `description` is the auto-invoke signal,
  so it must say what the skill does, the exact "use when" triggers, and what NOT to use it
  for. A vague description means the skill never fires or fires wrongly. (Same standard the
  librarian skill's own description meets.)
- **Shape** , narrow scope, concrete commands/paths, deterministic steps over generic
  advice; split a broad domain into several skills.
- **Where it goes** , the active authored-skills root: `/vault/.claude/skills/<name>/SKILL.md`
  when a vault is mounted (and then it follows vault discipline: a real artifact, logged like
  any vault mutation, not duplicating what a note should hold), or
  `~/.claude/skills/authored/<name>/SKILL.md` when there is no vault. The agent does not pick
  the root; it uses the one the system exposes.
- **Reserved names** , do not author `coding` or `librarian` (goclaw-owned).

## 5. Risks and limits (be honest)

- **Prompt bloat / skill sprawl.** An agent that authors skills too eagerly fills
  `~/.claude/skills/` with low-value or overlapping skills, every one's description competes
  for auto-invoke. Mitigation: the discipline's "only for a 2-3x repeated workflow" bar, and
  a periodic maintenance pass (we have the maintenance scheduler) that prunes stale/unused
  skills, same as vault lint.
- **A bad skill is a standing instruction.** Unlike a one-off mistake, a poorly-written
  agent-authored skill misfires on EVERY matching turn until removed. The operator can read
  and delete it (a vault file is git-tracked and visible in Obsidian; the no-vault fallback
  lives under `claude-home/skills/authored/`, host-readable but not git-tracked, a real reason
  to prefer a vault), and the prune-on-disappear composition cleans up. Worth stating in the
  discipline: a skill is a commitment, write it carefully.
- **NOT a capability escape.** A skill cannot do anything the agent could not already do, it
  is instructions, executed by the same sandboxed agent with the same tools. It does not
  grant host access, network reach, or new tools; those remain the plugin system's domain,
  operator-gated. This is the line that keeps the feature inside containment.
- **Reserved-name shadowing** (section 4a) must be enforced or an agent-authored `librarian`
  could override the real one.

## 6. Explicitly out of scope

- **A skill registry / external skill acquisition** (browse-and-install skills from the
  internet) is NOT proposed. Pulling external instructions the agent then follows is a
  capability-acquisition channel that belongs behind the same operator-gated, sandboxed,
  vetted path as plugins, not a registry the agent browses on its own. Agent-authored skills
  are the agent capturing ITS OWN workflows, no external fetch, which is why they are safe.
  If external skills are ever wanted, that is a separate RFC routed through the plugin-style
  install discipline, not this.

- **Searchable conversation recall** is NOT this RFC, and the distinction is worth stating
  because it is easy to assume the agent already has it. It does not. What exists today is:
  the claude CLI's own transcript jsonl under `claude-home/<id>/projects/` and `sessions/`
  (keyed by CLI session UUID, not by goclaw conversation, and not indexed for retrieval); the
  `inbound.db`/`outbound.db` per-conversation pair, which is the MESSAGE TRANSPORT boundary,
  not an archive; and the vault, which holds only what the agent DELIBERATELY files. So there
  is no automatic "save every conversation and search it back later" store, the agent
  remembers a past exchange only if it chose to write a vault note about it. Agent-authored
  skills do NOT depend on or provide that: a skill captures a repeated WORKFLOW (how to do a
  thing), not a searchable log of WHAT WAS SAID. Conversation recall is a genuinely separate,
  currently-missing capability (and arguably a stronger foundation for self-improvement); if
  wanted it is its own RFC, overlapping the event-log RFC on the operational side but distinct
  on the knowledge/conversation side. Flagged here so this RFC is not mistaken for delivering
  it.

## 7. Phasing

1. **Composition (4a):** scan the active skills root (vault dir when mounted, else
   `claude-home/skills/authored/`) and link every skill found, with reserved-name protection
   and the existing prune. This alone lets a hand- or agent-authored skill be discovered in
   BOTH the vault and no-vault configs. Small, testable (a root with two skill dirs yields two
   links; a reserved name does not shadow; the no-vault path links from claude-home).
2. **Discipline (4b):** ship a skill-creator skill that teaches when/how to author well.
   Delivery follows the config: the vault template (reaching fresh inits and existing vaults
   via `vault sync`) when a vault is used, and the baked image alongside `coding` so it is
   present even with no vault.
3. **(Later) maintenance prune:** a periodic pass that flags unused/duplicate agent-authored
   skills for the operator, reusing the maintenance scheduler.

## 8. Recommendation

Do 1 and 2 together: the composition change without the discipline lets skills be discovered
but gives the agent no guidance to author good ones (sprawl risk); the discipline without the
composition change is inert (authored skills are never linked). Make the no-vault root part of
phase 1, not an afterthought: skipping it silently disables self-improvement for a supported
config. Defer the prune (3) until there are enough agent-authored skills to need it. Keep it
strictly to agent-authored, host-resident, prompt-only skills; anything that wants to ACQUIRE
external capability or EXECUTE on the host stays in the plugin system, which is where the
vetting already lives. Searchable conversation recall (section 6) is explicitly NOT in this
RFC.
