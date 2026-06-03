# groups/

Per-agent-group filesystems (brief §3.3, §10). One subdirectory per agent
group, each holding that group's container-facing files:

```text
groups/<folder>/
  CLAUDE.md          # the group's agent instructions / schema
  skills/            # group-specific skills (skill-installed per fork)
  ...                # container config, mounts, etc.
```

These are **data, not Go code** - they are mounted into the group's container at
spawn time by `internal/runtime`. The `agent_groups.folder` column in the central
DB (`internal/db/schema.sql`) points at the subdirectory here.

`groups/` is intentionally empty in the scaffold; create a `groups/<name>/`
directory when you wire up the first agent group. If a group enables the optional
knowledge vault (brief §11), its `CLAUDE.md` is the vault schema - see
[`../vault-template/`](../vault-template/) for a starting point.
