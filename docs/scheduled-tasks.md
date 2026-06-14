# Scheduled tasks: user-definable recurring agent work

Status: SHIPPED. User-definable scheduled tasks are built as `internal/scheduler`
(distinct from `internal/maintenance`'s fixed vault-upkeep jobs): it loads tasks from
the central DB each tick and fires each into its OWN target (session/channel/chat).
The owner-gated `/schedule` command lives in the router (`internal/router/schedule.go`,
capped per owner), and a failed scheduled run is re-armed by the delivery path
(`RearmFailedSchedule`). So "send me a summary every morning at 8am" is a real,
persisted, restart-safe job. This doc is retained as the design record; the sections
below describe the machine that was built. (`docs/gmail-channel-plugin.md` covers tools
such a task would call.)

## 1. What already exists (most of it)

The load-bearing machine is built and running: `internal/maintenance.Scheduler`. Tracing
"do X every morning at 8am" against it:

- **`Job{Name, PeriodDays, AtHour, Every, Prompt}`** already expresses "daily at hour H,
  run this prompt." `AtHour:8, PeriodDays:1` IS "every morning at 8am."
- **`due(job, now)`** pins firing to a LOCAL wall-clock boundary (today at AtHour),
  records the last run in the central DB `kv` table, and fires at most once per period.
  So it survives restarts, never double-fires, and never drifts with host uptime. This
  is the genuinely fiddly part, and it is done and correct.
- **`fire(job)`** enqueues the prompt into a session's `inbound.db`, ensures the runner
  container is up, and records the run. The reply flows back through the normal delivery
  path to a channel/chat. So "wake the agent with an instruction and deliver its reply"
  already works end to end (it is what the `morning`/`nightly`/`weekly-health` jobs do).

And the WORK half exists too: `gmail-tools` exposes `gmail_search`/`gmail_read` as MCP
tools, so an agent woken with "summarize my unread inbox" can actually query Gmail.

So the capability is ~80% present. The gaps are about CONFIGURABILITY and TARGETING, not
the core scheduling.

## 2. The gaps (precise)

1. **The job list is hardcoded.** `DefaultJobs` is a fixed Go slice of vault-upkeep jobs.
   There is no way for a user (or the agent on a user's behalf) to ADD a job at runtime.
   Scheduling is operator-compile-time, not a runtime capability.
2. **One fixed target.** `Scheduler` holds a single `target Target` (the vault session).
   Every job fires into that one session and delivers there. A per-user "inbox summary to
   MY Telegram" needs each job to target an arbitrary session/channel/chat. The mechanism
   supports it (`Target` carries Channel/ChatID), but it is wired to one target at
   startup.
3. **No agent-facing way to create one.** A user telling the agent "do this every
   morning" does nothing: the agent has no tool to register a schedule. This is the
   surface that makes the feature feel real ("just ask").

## 3. The design: a general scheduler; maintenance becomes one consumer

Generalize "fixed maintenance jobs against one target" into "arbitrary scheduled tasks,
each with its own target." The vault-maintenance jobs become DEFAULT tasks seeded into
the same store, not a separate hardcoded path.

### 3a. Persist tasks in the DB, not in Go

A new `scheduled_tasks` table (central DB) is the source of truth, so tasks are
user-editable and survive restarts:

```text
scheduled_tasks
  id            TEXT PRIMARY KEY        -- uuid
  name          TEXT                    -- human label, unique per owner
  owner_user_id INTEGER                 -- who created it (FK users); authorization + listing
  agent_group_id INTEGER                -- where it runs
  session_key   TEXT                    -- which session (so multi-turn context is its own)
  channel       TEXT                    -- delivery channel for the reply
  chat_id       TEXT                    -- delivery chat
  period_days   INTEGER                 -- 1 = daily, 7 = weekly
  at_hour       INTEGER                 -- local hour 0-23; <0 = use every_seconds
  every_seconds INTEGER                 -- pure-interval fallback when at_hour < 0
  prompt        TEXT                    -- the instruction enqueued for the agent
  enabled       INTEGER NOT NULL DEFAULT 1
  created_at    TEXT
```

The `maintenance.Job` fields map 1:1 (period_days/at_hour/every/prompt), plus the per-task
TARGET (agent_group/session/channel/chat) and OWNER. Last-run tracking stays in `kv`
keyed by task id (the existing `due()` logic is reused verbatim).

### 3b. The scheduler loops over DB tasks, not a Go slice

`Scheduler.tick` already iterates `s.jobs`. Change it to load enabled tasks from
`scheduled_tasks` each tick (cheap; a handful of rows), run the SAME `due()` check per
task (keyed by task id in kv), and `fire()` into the TASK's own target (not a single
shared one). `fire()` becomes per-task-target instead of per-scheduler-target, a small
change: it already takes the target's pieces, they just come from the task row now.

So the diff is:
- `Job` gains a target + owner (or a new `Task` type wrapping it); `due()` unchanged.
- `tick()` loads tasks from the DB instead of the fixed slice.
- `fire()` reads the target from the task, not `s.target`.
- Vault maintenance: the `DefaultJobs` are SEEDED into `scheduled_tasks` on first run
  (idempotent, like the bootstrap owner), targeting the vault session as today. They stop
  being a special code path and become ordinary rows. (Or kept as a separate built-in set
  the scheduler also evaluates; decide in build, seeding is cleaner.)

### 3c. The agent-facing surface: a `schedule` tool (the key new thing)

This is what makes "ask the agent to do it every morning" work. The agent gets a built-in
tool (an MCP tool the in-container runner exposes, like a plugin's tools, but host-backed,
see 3d) to manage the owner's scheduled tasks:

- `schedule_create(name, prompt, at_hour, period_days, deliver_to?)` -> creates a task,
  defaulting the target to the CURRENT conversation's session/channel/chat (so "summarize
  my inbox every morning" delivered to where you asked). Returns the task id.
- `schedule_list()` -> the owner's tasks.
- `schedule_delete(name|id)` / `schedule_enable` / `schedule_disable`.

So the flow for the motivating example:

```text
You (Telegram): "Send me a summary of my unread inbox every morning at 8am."
Agent: calls schedule_create(
         name="inbox-summary",
         prompt="Summarize my unread Primary inbox: use gmail_search/gmail_read,
                 give me a tight bulleted digest.",
         at_hour=8, period_days=1,
         deliver_to=<this Telegram chat>)
       -> "Done, I'll send an inbox summary here every morning at 8am."

Every day at 08:00 local:
  scheduler fires the task -> enqueues the prompt into the session -> agent runs,
  calls gmail_search + gmail_read, writes the digest -> delivered to your Telegram.
```

There would ALSO be a human `/schedule` slash command (list/add/remove) for direct
control, but the tool is the part that makes it conversational.

### 3d. Where the schedule tool lives (host-backed, not a plugin)

Unlike `gmail-tools` (an installed plugin), the schedule tool acts on goclaw's OWN DB
(`scheduled_tasks`), so it is a FIRST-PARTY tool the host backs, not untrusted plugin
code. Two ways to expose it to the agent, decide in build:

- The in-container runner exposes it as an MCP tool whose handler calls back to the host
  (over the boundary) to read/write `scheduled_tasks`. Reuses the channel boundary work.
- Or simpler first cut: the host intercepts `schedule_*` as a pass-through command set
  (like `/reset`), no agent tool, the user types `/schedule ...`. Less magical but no new
  boundary call.

Recommendation: ship the `/schedule` command first (no new plumbing, immediately useful),
then add the agent tool so it is conversational. Both read/write the same table.

## 4. Authorization and safety (this is a scheduler that runs arbitrary prompts)

A scheduled task enqueues an arbitrary prompt that wakes the agent with the owner's
tools. So it is a privilege surface and must be gated:

- **Owner/known-user only.** `schedule_create` is allowed only for a known user (the
  access gate already establishes this), and the task records `owner_user_id`. A task
  delivers ONLY to a chat the owner is authorized for (do not let a task created from one
  channel deliver to another user's chat). Reuse the delivery-authorization check.
- **No privilege escalation via the prompt.** The task prompt runs as the agent with
  whatever tools/credentials the agent has, same as any message. It does not grant new
  capability; it just triggers the agent on a timer. The risk is a user scheduling
  something abusive against THEIR OWN agent, which is their call. Cross-user is the line:
  a task's target must belong to its owner.
- **Rate / sanity caps.** A minimum interval (no every-10-seconds task hammering the
  agent and burning tokens), and a per-owner task count cap. Fail closed on a malformed
  schedule (bad hour, empty prompt).
- **Disabled tasks stay in the table** (enabled=0) so "pause" is not "delete"; a paused
  task never fires.

## 5. Failure modes (the scheduler must be robust; your morning job already exposed some)

- **Host down at the boundary.** If the host is off at 08:00 and starts at 08:30, the
  wall-clock `due()` still fires once for that day (now >= boundary, not yet run today).
  Good, already the behavior. If the host is down ALL day, the task is simply missed for
  that day (no catch-up storm); acceptable for a digest.
- **Agent/network failure at fire time** (cf. the live `mitm handshake EOF` flap). `fire`
  enqueues the prompt and records the run BEFORE the agent processes it, so a transient
  agent failure does not re-fire the task (the prompt is durably queued in inbound.db and
  processed when the agent recovers). The reply may be late, not lost. NOTE: this means a
  task does not retry its PROMPT on agent failure, it relies on the queued message being
  consumed eventually. That is the right default for a daily digest; a task that must
  retry would need explicit retry semantics (out of scope now).
- **Time zone.** Boundaries are the host's LOCAL zone (today the maintenance jobs already
  assume this). A task's `at_hour` is host-local; document it. Per-task zones are a future
  nicety, not now.
- **A task whose target session/channel no longer exists** (e.g. the user removed the
  wiring). `fire` should skip-and-log rather than error the whole tick, and ideally
  auto-disable a task that fails to deliver N times.

## 6. What changes, concretely

- **Migration:** `scheduled_tasks` table (3a).
- **`internal/maintenance` -> generalize (or a new `internal/scheduler`):** load tasks
  from the DB each tick; `due()` reused per task id; `fire()` targets the task's own
  session/channel/chat. Seed `DefaultJobs` as rows on first run.
- **`/schedule` command** (`internal/command` + router): create/list/remove/enable, owner
  -gated, defaulting target to the current conversation.
- **(Phase 2) agent `schedule_*` tool:** host-backed MCP tool so it is conversational.
- **Tests:** due-at-boundary + no-double-fire (reuse the maintenance tests, now per task);
  a task fires into its OWN target, not a shared one; owner-gating (a non-owner cannot
  create/see another's task); a task delivering to a chat the owner is not authorized for
  is rejected; min-interval and count caps; disabled tasks never fire.

## 7. Scope discipline / phasing

1. **Phase 1 (unlocks the motivating feature):** the table + generalized scheduler +
   `/schedule` command. With this, the OPERATOR (or owner via `/schedule`) can create
   "8am inbox summary delivered to my Telegram", and it runs. The Gmail tools already
   exist; the credential/OAuth path (`docs/oauth-credentials.md`) must be working for the
   agent to actually read Gmail, that is the real external dependency, not the scheduler.
2. **Phase 2 (makes it conversational):** the agent-facing `schedule_*` tool, so "just
   ask" works without the user knowing `/schedule` syntax.
3. **Deferred:** per-task time zones, prompt-level retry semantics, catch-up policy
   richer than "fire once at/after the boundary", cron-grade expressions (the
   period_days + at_hour model covers daily/weekly; a full cron string is more than the
   common case needs, add only if a real case demands sub-daily or day-of-week).

The discipline mirrors the rest of the system: the hard, correctness-critical part
(restart-safe wall-clock firing, no double-fire) ALREADY EXISTS and is reused verbatim;
the new work is configurability (a table + a command + a tool) and per-task targeting,
which are mechanical on top of the proven core.
