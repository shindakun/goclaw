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
has passed is stale and reclaimable — read the prior notes below first. If you
can't finish, set `state: blocked`, fill `blocked_on`, clear the claim, and log a
handoff. Finished work becomes `state: done`; never delete the note.

## Notes / handoff
<!-- append progress notes here; the newest at the bottom, dated. -->
