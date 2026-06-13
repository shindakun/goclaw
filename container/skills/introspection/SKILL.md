---
name: introspection
description: Diagnose goclaw's own behavior from the operational event log at /run/goclaw/events/. Use when something operational went wrong or looks off: a scheduled task that did not run, deliveries failing or being denied, the proxy CA churning, a runner that relaunched or was reaped, a plugin install/remove you need to confirm, or auditing your own recent operations. Advisory and read-only: this skill READS the event log and reports findings to the owner chat or a vault note (or, for a real code bug, a PR branch); it NEVER writes the event log, changes host config, or restarts anything. Do NOT use for knowledge work (use librarian) or one-off chat.
---

# Introspection

Your operations leave a trace. The host writes an append-only, structured event log of what
the SYSTEM actually did; you read it to diagnose problems and recommend fixes to the owner. You
read it; you never write it (the host is the only writer, and the mount is read-only). You are
advisory: you cannot change the running system yourself (see "Find the WHAT" below).

## Source-of-truth hierarchy (when sources disagree about an operation)

1. **The event log** (`/run/goclaw/events/event-log.jsonl`) - operational FACT. Ground truth
   for what the host/runner did.
2. **The channel / conversation** - what was actually SENT/received. Beats your recollection.
3. **Vault notes** - your own INTERPRETATION. Narrative, not operational fact.
4. **Your current beliefs** - may be stale. Lowest.

Trust higher over lower: event log > channel history > vault > beliefs. The VAULT is ground
truth for KNOWLEDGE; the event log is ground truth for OPERATIONS. Do not conflate them.

## The log: one file, one event per line, discriminated by `kind`

```json
{"ts":"2026-06-09T07:00:03-07:00","kind":"schedule.deferred","ok":false,"fields":{"task":"inbox","reason":"ensure runner: outage"}}
{"ts":"2026-06-09T07:05:03-07:00","kind":"schedule.fired","ok":true,"fields":{"task":"inbox","owner":7}}
{"ts":"2026-06-09T07:05:09-07:00","kind":"delivery.sent","ok":true,"fields":{"session":"telegram:42","channel":"telegram","chat":"42","msg_id":91}}
```

Kinds you will see: `schedule.fired` / `schedule.deferred`; `delivery.sent` / `delivery.denied`
/ `delivery.failed`; `proxy.ca_generated`; `plugin.install` / `plugin.remove`;
`runner.launched` / `runner.reaped`. Common fields: `ts` (RFC3339 local), `kind`, `ok`
(present where success/failure is meaningful), and `fields` (kind-specific). There are no
message bodies or secrets here by design; do not expect them.

## How to query (diagnosis is a filter, not a hunt)

```bash
LOG=/run/goclaw/events/event-log.jsonl

# Last 20 events.
tail -n 20 "$LOG" | jq .

# Did my 07:00 task hand off, or defer? (the "fired but didn't complete" class)
jq -s 'map(select(.kind|startswith("schedule."))) | sort_by(.ts)' "$LOG"

# All failures/denials.
jq -c 'select(.ok == false)' "$LOG"

# Events by kind, counted (what is happening most).
jq -s 'group_by(.kind) | map({kind:.[0].kind, n:length}) | sort_by(-.n)' "$LOG"

# Deliveries to a channel that keep failing.
jq -c 'select(.kind=="delivery.failed") | .fields.channel' "$LOG" | sort | uniq -c

# Did the proxy CA churn (the bad-cert-flood signature)?
jq -c 'select(.kind=="proxy.ca_generated")' "$LOG"

# Did a runner keep relaunching (a crash loop)?
jq -c 'select(.kind=="runner.launched")' "$LOG"
```

If the log has rotated there is also `event-log.1.jsonl` in the same dir; read both (oldest
first) when a question reaches back past the active file.

## Find the WHAT, then say how the SYSTEM should change

When you find something wrong, do not stop at the symptom: ask why the SYSTEM produced it. But
be clear-eyed about what you can actually DO from here. You are in a sandbox: you cannot reach
the host, you cannot change goclaw's config or restart anything, and the event log is read-only.
You do NOT fix the system directly. What you produce is one of:

1. **A note to the owner** with the evidence and the recommended change. This is the default and
   often the only appropriate output (anything needing host access: restarting runners, editing
   `.env`, changing a schedule, adding a delivery authorization).
2. **A vault note** recording your interpretation (when a vault is mounted), so the finding is
   not lost. Knowledge, not operations.
3. **A pull request against the goclaw source**, but ONLY when the fix is a genuine code change
   and you have a GitHub token. The repo is `https://github.com/shindakun/goclaw`. Use the
   coding skill: clone into `/work`, make the change on a BRANCH (never commit to or push
   `main`), and open a PR for the owner to review and merge. You never deploy it yourself; a
   merged PR still needs the owner to rebuild and restart.

Examples, with the realistic output:

- `schedule.deferred` with no later `schedule.fired` for the same task => the task is stuck (the
  runner never came back). Output: a note to the owner with the task name and the deferral
  reason. NOT "I will remember to check," and NOT a config edit (you cannot make one).
- Repeated `delivery.denied` for one target => an authorization gap. Output: tell the owner the
  exact channel/chat from `fields` and the destination rule that is missing.
- A `proxy.ca_generated` you did not expect => running containers trust a stale CA. Output: tell
  the owner to recreate runners (the documented remedy); you cannot do it.
- Repeated `runner.launched` for one group with little `runner.reaped` between => a crash loop.
  Output: gather the surrounding failures and surface them to the owner. If the root cause is a
  bug in goclaw itself and you have a token, a PR (on a branch) is appropriate.

A root-cause pass that ends without a concrete owner message, a vault note, or a PR did not
happen. Do not claim you "fixed" or "changed" anything you could not actually do from the
sandbox; say what you found and what the owner (or a PR) should do.

## Boundaries

You READ the event log and WRITE to the vault / the owner chat / a PR branch. You never write
the event log (read-only mount; the host is the sole writer), you never push to `main`, and you
gain no ability to act on the host or deploy a change. Your power here is diagnostic and
advisory, not a control surface.
