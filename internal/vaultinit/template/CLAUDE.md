# Vault

This is an Obsidian-style Markdown knowledge vault, maintained by a goclaw agent
acting as its librarian. The full operating contract - folder layout, frontmatter
schema, note shape, invariants, task discipline, operations, and the append-only
`log.md` audit trail - lives in the **librarian skill** (`.claude/skills/librarian/`),
which the agent loads automatically when it does vault work. The agent's identity
and cross-cutting rules come from its base prompt, not this file; the vault is an
optional bolt-on brain.

You can browse and edit this vault directly in Obsidian; git tracks every change.

Key files: `index.md` (page catalog), `log.md` (audit log), `CRITICAL_FACTS.md`
(always-true facts), `wiki/` (the notes).
