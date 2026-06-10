# Self-improvement ladder: agent-authored skills and operator-gated plugins (RFC)

Status: DESIGN / RFC. Nothing built. Date: 2026-06-10. Proposes one coherent design for how
the agent improves its OWN capabilities over time, with the authorization gate scaling to the
blast radius of the change. Two rungs:

1. **Skills** (prose): the agent captures a repeated workflow as a `SKILL.md`. It lands
   directly, no human gate, because a skill is instructions, not code, and adds no execution
   surface. (Detailed separately in `docs/agent-authored-skills.md`; summarized here as rung 1.)
2. **Plugins** (code): the agent AUTHORS a plugin but cannot install it. It PROPOSES; the
   operator approves; the existing vetted, sandboxed `plugin add` path runs. The agent
   proposes, a human pulls the trigger.

The governing principle: **the gate scales with the blast radius.** A prose self-edit changes
what the agent reads; it lands free. A code self-edit changes what RUNS in the box; it routes
through a human. Same loop ("the agent gets better at its job"), two very different rungs.

## 0. The containment constraint (this is the whole design, not a footnote)

The agent is untrusted (prompt injection; the box must not be escapable, file or network). The
plugin trust model exists PRECISELY because plugin code is untrusted: today a plugin is cloned,
scanned, and built in a throwaway container, only the binary + plugin.yml are copied out, and
even then it runs sandboxed with a minimal-env allowlist behind the credential proxy. That
entire apparatus assumes a HUMAN OPERATOR decided to install it: `goclaw plugin add <url>` is
owner-only, gated at message-routing time before the agent ever sees the command
(`internal/router/router.go`, MinRole RoleOwner; the agent cannot emit `/plugin add` and have
it run, the router's `Intercept` only handles `/schedule`).

If the AGENT could author AND install a plugin, it would close the exact loop the boundary
exists to keep open: the untrusted thing inside the box would decide what new code runs in the
box. Even sandboxed, that is the agent acquiring new capability on its own initiative, which is
the thing containment forbids. So the line is NOT "skills good, plugins bad." It is **who
authorizes the new capability:**

- A skill adds no new capability (it is a better prompt for tools the agent already has). No
  gate needed.
- Authoring plugin code is fine; writing Go text is not a capability.
- INSTALLING/RUNNING that code must stay operator-gated. The agent proposes; a human approves.

The user's own framing said it best: the agent improving itself should look like **a
contributor proposing a change, not an admin applying one.** For a plugin, that is literally a
PR, or its in-system equivalent (a proposal the operator approves). For a skill, no PR is
needed because there is nothing to execute.

## 1. Rung 1: agent-authored skills (prose, lands directly)

Covered in full by `docs/agent-authored-skills.md`; the relevant points for the ladder:

- A skill is a `SKILL.md` the CLI auto-invokes by its `description`. It is PROMPT text the
  agent reads, executed by the same sandboxed agent with the same tools. It grants no host
  access, no network reach, no new tools.
- The agent can already write one (the vault is RW-mounted; the no-vault fallback is
  `claude-home/skills/authored/`). The only gap is composition discovering it
  (`composeGroupPrompt` currently links only the hardcoded `coding` + `librarian`).
- Because it adds no execution surface, it needs no approval gate. It lands directly, exactly
  like the agent writing a vault note. The operator can read and delete it (a vault skill is
  git-tracked and Obsidian-visible) if it misbehaves; a bad skill is a standing instruction,
  not a capability escape.

This is the cheap, safe rung and should ship first. It is the proof that "the agent gets
better at recurring work" does not REQUIRE crossing the code boundary, most self-improvement is
prose.

## 2. Rung 2: operator-gated plugin proposals (code, human approves)

When a workflow genuinely needs a new TOOL (host execution: a new API the agent can call, a
computation a skill cannot express), a skill is not enough, that is what plugins are for. The
agent may AUTHOR such a plugin but must not install it. The shape:

### 2a. The agent authors and proposes (never installs)

- The agent writes the plugin source (Go, importing goclawkit, the same shape any plugin has).
  Where it writes is an open question (section 4): a vault subtree, or a dedicated proposals
  area. It is just text the agent produces; writing it crosses no boundary.
- The agent then PROPOSES it, the in-system equivalent of opening a PR. Concretely a new
  agent-emittable directive, intercepted host-side exactly as `/schedule` is today
  (`internal/router/schedule.go` Intercept): an agent reply that IS a `/plugin propose <url>`
  (or `<path-to-authored-source>`) is caught by the host, recorded as a PROPOSAL, and NOT sent
  to the user as raw text. The agent cannot reach `installer.Add`; it can only file a proposal.

### 2b. The proposal is a durable, operator-visible record

Mirror the EXISTING `pending_approvals` pattern (`internal/db/approvals.go`): a host-owned
`plugin_proposals` table (id, source ref, rationale the agent gave, agent_group, created_at,
status pending|approved|rejected). Host is the sole writer (single-writer-per-cross-mount-file
rule; this is a central-DB row, not a cross-mount file, so it is trivially safe). The proposal
is the contributor's PR: it sits, visible, until a human acts.

### 2c. The operator approves, and ONLY THEN the vetted path runs

- A new owner-only command `goclaw plugin proposals` / `plugin approve <id>` / `plugin reject
  <id>`, gated owner-only at routing time exactly like `/plugin add` is now.
- `plugin approve <id>` reads the stored source ref and runs the EXISTING `installer.Add`
  unchanged, the same clone-scan-build-in-throwaway-container, copy-artifact-out, accept flow,
  with the same security scan and the same atomic install. Nothing about the trust pipeline
  weakens; we are only adding a human-gated TRIGGER in front of it, sourced from an agent
  proposal instead of a hand-typed URL.
- Approval is the human-in-the-loop gate. The agent did the authoring work; the operator
  pulled the trigger. This is the PR-merge moment.

### 2d. What this explicitly does NOT do

- It does NOT let the agent install, run, or auto-approve anything. No code the agent wrote
  runs until a human approves it, and even then it runs through the unchanged sandbox.
- It does NOT weaken the build/scan/sandbox pipeline. The scan stays a best-effort deterrent
  (the real protection remains the env allowlist + credential proxy, as today); a human
  approving a proposal is accepting the same trust they accept typing `plugin add <url>`.
- It does NOT give the agent a live channel to the host. A proposal is a durable record the
  host reads on its own schedule, not an RPC; the agent writes a directive into its outbound
  reply (the existing pull-only boundary), the host intercepts it. No new socket, no new
  write path into the host.

## 3. Why this shape (and not the alternatives)

- **Why not let the agent self-install sandboxed plugins?** Because "sandboxed" bounds the
  blast radius of code that runs; it does not change WHO decided to run it. Self-install is the
  agent acquiring capability autonomously, the precise thing containment refuses. The sandbox
  is defense-in-depth UNDER the human gate, not a replacement for it.
- **Why a proposal table and not just a PR to the repo?** A real PR is a perfectly legitimate
  expression of this (and for goclaw's OWN code, the right one, the agent clones, branches,
  opens a PR, a human reviews and merges). The in-system proposal table is the same idea for
  the common case where the operator runs goclaw and wants to approve from chat without a
  GitHub round-trip. Both are "contributor proposes, human approves"; the table is the
  low-friction local form. They can coexist: repo PRs for goclaw-core changes, proposals for
  plugins the operator installs into their own instance.
- **Why mirror `pending_approvals`?** It already encodes exactly this lifecycle (a held thing,
  an owner-only approve, a delete on resolution) and is single-writer host-owned. Reusing the
  pattern keeps one mental model for "agent requests, owner approves" across access and
  plugins.

## 4. Open questions

1. **Where does authored plugin source live before approval?** A vault subtree (RW, git-tracked,
   operator-visible, but mixes code into a knowledge store) vs a dedicated host proposals dir
   (cleaner separation, new mount). Leaning: a dedicated agent-writable proposals dir that the
   approve path reads, NOT the vault, so plugin CODE never lives in the knowledge vault. Must
   confirm the installer can be pointed at a local source path, not only a git URL (today
   `Add` takes a git URL and clones; a local-path proposal may need the source pushed to a repo
   first, or `Add` extended to accept a vetted local dir, which itself must be built in the
   sandbox, never on the host).
2. **Git URL vs local source.** Simplest first cut: the agent proposes a GIT URL (it authored
   the plugin in its own repo and pushed it), and approve runs the unchanged `Add`. This avoids
   the host ever reading agent-authored code directly, the source still gets cloned + scanned +
   built in the throwaway container. A later cut could let the agent propose local source, but
   that local source must STILL be built in the sandbox, not trusted as-is.
3. **What rationale does the agent attach?** The proposal should carry the agent's case (why
   this tool, what workflow it serves, what it calls) so the operator can judge without reading
   all the code. This is the PR description.
4. **Rate / scope limits.** An agent that proposes plugins constantly is noise. A cap, or a
   "one open proposal at a time" rule, keeps it sane. Report, do not silently drop.
5. **Skill-first discipline.** The agent should reach for a SKILL before a plugin: most
   recurring work is a better prompt, not a new tool. The skill-creator discipline (rung 1)
   should say "propose a plugin only when host execution is genuinely required, a skill cannot
   express it." Otherwise rung 2 gets over-used for things rung 1 handles free.

## 5. Phasing

1. **Rung 1 (skills):** build `docs/agent-authored-skills.md`, composition scans the skills
   root + the skill-creator discipline. Safe, no gate, proves most self-improvement is prose.
2. **Rung 2 proposal plumbing:** the `plugin_proposals` table (mirroring `pending_approvals`),
   the agent-emittable `/plugin propose <git-url>` directive (intercepted host-side like
   `/schedule`), and the owner-only `plugin proposals` / `approve` / `reject` commands.
   `approve` runs the UNCHANGED `installer.Add`. Git-URL-only first (open question 2).
3. **(Later) local-source proposals:** let the agent propose authored source directly, still
   built in the sandbox, never trusted on the host. Only if git-URL proposals prove too
   indirect in practice.
4. **(Later, separate) repo PRs for goclaw-core:** the agent cloning goclaw, branching, and
   opening a PR for changes to goclaw ITSELF (not plugins). This is the legitimate
   "contributor" path for core changes and is a different track from plugin proposals; it needs
   its own design (credentials to push, which repo, review discipline) and is out of scope here
   beyond noting it is the natural top of the ladder.

## 6. Recommendation

Ship rung 1 (skills) first and on its own: it is safe, small, and delivers most of the
self-improvement value without touching the code boundary. Do rung 2 (plugin proposals) only
when there is a real recurring need for a TOOL a skill cannot express, and do it git-URL-first
so the host never reads agent-authored code directly, the existing sandbox pipeline does all
the vetting, with a human approval as the only new gate. Keep the principle visible in both the
code and the agent's own discipline: **prose lands directly; code routes through a human.** The
agent proposes; the operator approves. Anything that would let the agent run code it wrote
without a human in the loop is off the table, that is the containment line this whole ladder is
built to respect.
