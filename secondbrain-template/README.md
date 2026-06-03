# Second Brain — starter vault template

A ready-to-use skeleton for the optional second-brain layer described in
[`../nanoclaw-go-podman-brief.md`](../nanoclaw-go-podman-brief.md) §11. A shared,
Obsidian-style Markdown vault that both you and the agent read and write: you browse
and edit it in Obsidian, the agent maintains it. The vault compounds — each source is
read once, distilled, and merged into existing notes, so synthesis and cross-links are
already there next time.

## What's here

```text
CLAUDE.md          # the operating manual / behavioral contract — read on every session
index.md           # page catalog — the agent reads this first
log.md             # append-only, greppable activity log
CRITICAL_FACTS.md  # tiny always-loaded facts (fill these in)
raw/               # immutable sources you drop in (agent reads, never edits)
wiki/
  entities/        # people, companies, tools   (+ one example note)
  concepts/        # ideas, frameworks, synthesis (+ one example note)
  projects/        # ongoing work
  decisions/       # decision records, the "why" (+ one example note)
  resources/       # links, documents, references (+ one example note)
  credentials/     # where secrets live — never the secret (+ one example note)
  daily/           # day notes (YYYY-MM-DD.md)
  tasks/           # open / closed tasks
```

The `_example-*.md` files show the required note shape and frontmatter. Delete them
once you have real notes.

## Setup

1. Copy this directory to your vault location (the brief uses `~/SecondBrain/`):

   ```sh
   cp -R secondbrain-template ~/SecondBrain
   cd ~/SecondBrain && git init && git add -A && git commit -m "init vault"
   ```

2. Fill in `CRITICAL_FACTS.md` (owner, purpose, timezone).
3. Open the folder in Obsidian — the graph view and Dataview work out of the box.
4. Wire it into a goclaw agent group: add `~/SecondBrain` as a **read-write**
   mount-allowlist entry mounted at `/vault` (`:Z` under Podman), and point the group's
   container `CLAUDE.md` at this manual. See §11.5 of the brief for the `flock` write
   guard and scheduled-maintenance jobs.

## Why git

Every agent edit becomes a reviewable, revertible diff — the safety net for the one
place the vault crosses session isolation (§11.6). Keep the vault scoped to a **single
agent group**; never share one vault across groups.
