---
type: task
state: open
date: 2026-06-01
domain: [example]
tags: [example]
trust: trusted
confidence: stated
entities: []
unresolved_reference:
claimed_by:
lease_until:
blocked_on:
---

Example task note. Preamble: what needs doing, in one or two sentences, so a
worker can judge it before reading on. Delete this file once real tasks exist.

## Detail
What "done" looks like, and any context or links ([[example-project]]) a worker
needs to pick it up cold.

## Claiming
Before working, set `state: claimed`, `claimed_by: <you>`, and `lease_until` to a
short horizon, then append a `claim` line to `log.md`. A claim whose `lease_until`
has passed is stale and reclaimable - read the prior notes below first. If you
can't finish, set `state: blocked`, fill `blocked_on`, clear the claim, and log a
handoff. Only set `state: done` once the task's real deliverable has actually
happened (message sent / file written / command run) - a summary that *says* you
did it is not doing it. Never delete the note.

## Notes / handoff
<!-- append progress notes, newest at the bottom, each stamped `YYYY-MM-DD HH:MM`
     so a status line is true AT THAT TIME, e.g.:
     - 2026-06-01 09:14 — filed by agent:librarian; not yet claimed.
     - 2026-06-01 09:40 — claimed by agent:librarian, lease_until 2026-06-01 10:10. -->
