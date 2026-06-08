# Agent-authored skills (RFC)

Status: DESIGN / RFC. Nothing built. Date: 2026-06-07. Proposes letting the agent capture a
repeated workflow as a new reusable skill (a `SKILL.md`) that the system then auto-discovers
on later turns, so the agent improves its own capabilities over time within the existing
sandbox. Read `internal/runtime/compose.go` (skill composition) and the librarian skill
template first.

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
`description`. Two skills ship today:

- `coding` , baked into the image at `/app/skills/coding`, always linked.
- `librarian` , provided by the vault at `/vault/.claude/skills/librarian`, linked only when
  a vault is mounted.

The agent CAN already write into the vault (it is mounted read-write at `/vault`), so it can
physically create `/vault/.claude/skills/<new>/SKILL.md` today. **The gap is discovery:**
`composeGroupPrompt` hardcodes the ONE vault skill it links (`vaultLibrarianSkillPath`), so a
newly-authored vault skill is never symlinked in and the CLI never sees it. So agent-authored
skills are two small pieces away: (a) link ALL vault skills, not just `librarian`; (b) a
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

## 3. Where agent-authored skills live

In the VAULT, under `.claude/skills/<name>/SKILL.md`, alongside `librarian`. Reasons:

- The vault is the agent's read-write, git-tracked, durable store; a skill it authors is
  exactly that kind of artifact (a captured, reusable piece of knowledge-about-how-to-work).
- It survives container bounces and is visible/editable by the operator.
- It rides the existing vault mount and composition, no new mount, no new host write path.

NOT in `/app/skills/` (baked into the image, read-only, ships with goclaw) and NOT a new
host directory. Image skills are goclaw's; vault skills are the agent's/operator's. This
mirrors the ownership split we already use for the librarian skill and the `vault sync`
command (goclaw owns the contract files; the vault owns the rest).

## 4. The two pieces

### 4a. Composition: link ALL vault skills (the enabling change)

`composeGroupPrompt` should, when a vault is mounted, scan `/vault/.claude/skills/` and
symlink EVERY subdirectory that contains a `SKILL.md` into `~/.claude/skills/`, not just the
named `librarian`. Concretely:

- Replace the single `desired["librarian"] = vaultLibrarianSkillPath` with a scan of the
  vault skills dir (on the HOST path, since compose runs host-side) for `*/SKILL.md`, adding
  one symlink per skill dir whose target is the CONTAINER path `/vault/.claude/skills/<name>`
  (the symlink dangles on the host, resolves in the container, exactly as the librarian link
  does today).
- The existing prune logic still removes links for skills that no longer exist, so deleting
  an agent-authored skill (or unmounting the vault) cleans up its link.
- `coding` (image skill) is unchanged and always linked.

Caveat: `librarian` is special, it is goclaw-owned (shipped in the vault template, refreshed
by `vault sync`). The scan should still link it; it is just no longer the ONLY vault skill
linked. A name collision (an agent-authored skill named `librarian` or `coding`) must NOT
shadow the goclaw-owned one, the composition should treat the goclaw-owned names as reserved.

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
- **Where it goes** , `/vault/.claude/skills/<name>/SKILL.md`, and that authoring one is a
  vault write, so it follows vault discipline (it is a real artifact, log it like any vault
  mutation; do not duplicate what a note should hold).
- **Reserved names** , do not author `coding` or `librarian` (goclaw-owned).

## 5. Risks and limits (be honest)

- **Prompt bloat / skill sprawl.** An agent that authors skills too eagerly fills
  `~/.claude/skills/` with low-value or overlapping skills, every one's description competes
  for auto-invoke. Mitigation: the discipline's "only for a 2-3x repeated workflow" bar, and
  a periodic maintenance pass (we have the maintenance scheduler) that prunes stale/unused
  skills, same as vault lint.
- **A bad skill is a standing instruction.** Unlike a one-off mistake, a poorly-written
  agent-authored skill misfires on EVERY matching turn until removed. The operator can read
  and delete it (it is a git-tracked vault file), and the prune-on-disappear composition
  cleans up. Worth stating in the discipline: a skill is a commitment, write it carefully.
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

## 7. Phasing

1. **Composition (4a):** link all vault skills, with reserved-name protection and the
   existing prune. This alone lets a hand-authored or agent-authored vault skill be
   discovered. Small, testable (a vault with two skill dirs yields two links; a reserved name
   does not shadow).
2. **Discipline (4b):** ship a skill-creator skill in the vault template (reaches fresh
   inits and existing vaults via `vault sync`), teaching when/how to author well.
3. **(Later) maintenance prune:** a periodic pass that flags unused/duplicate agent-authored
   skills for the operator, reusing the maintenance scheduler.

## 8. Recommendation

Do 1 and 2 together: the composition change without the discipline lets skills be discovered
but gives the agent no guidance to author good ones (sprawl risk); the discipline without the
composition change is inert (authored skills are never linked). Defer the prune (3) until
there are enough agent-authored skills to need it. Keep it strictly to agent-authored,
in-vault, prompt-only skills; anything that wants to ACQUIRE external capability or EXECUTE
on the host stays in the plugin system, which is where the vetting already lives.
