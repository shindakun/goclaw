# Log

Append-only activity log. Keep each entry on one line, prefixed `## [` so it stays
greppable: `grep '^## \[' log.md | tail`.

Format:
## [YYYY-MM-DD HH:MM] <ingest|query|reconcile|synthesize|lint> — <one line> — pages: a.md, b.md

<!-- entries below, newest at the bottom -->
