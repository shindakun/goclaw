# Task work: long-running goals as host-orchestrated task runs (RFC)

Status: DESIGN / RFC. Nothing built. Date: 2026-06-14. Sketches how goclaw could grow
from "answer a message" to "pursue a multi-step goal over time" without weakening the
containment boundary or inventing a second architecture. The thesis: a task system is a
HOST-SIDE orchestration layer composed from primitives goclaw already has, not a new
runtime. Every OPEN QUESTION at the end needs an owner decision before building; this is a
direction, not a plan of record.

## 1. What "task work" means here

Today goclaw is turn-shaped: a message (from a user, or the scheduler/maintenance) becomes
one inbound row, the runner answers it in one logical turn, the host delivers the reply.
Great for "summarize my inbox", wrong shape for "migrate this service to the new API",
which is many steps, spans hours or days, needs checkpoints, and should not trust the
agent's own "done".

Task work adds a third inbound origin alongside `user` and the scheduler: a **goal** the
host decomposes into a sequence (or DAG) of **tasks**, runs one at a time as ordinary
runner turns, verifies each from OUTSIDE the runner, checkpoints between them, and asks the
owner only at the gates that need a human. The agent still just answers prompts; the
host owns the loop, the state, and the gates.

## 2. The load-bearing claim: goclaw already has the primitives

This is why task work is additive, not a rewrite. Each thing a task orchestrator needs
already exists in the codebase:

| Orchestrator need | Already in goclaw |
| --- | --- |
| Host enqueues a prompt, runner executes, host delivers | the scheduler's `fire()`: ResolveOrCreateSession -> EnsureRunner -> EnqueueInboundSource -> deliver |
| Fresh context per unit of work | one session = its own multi-turn context; a task can get its own session key |
| "Fired but the turn failed must not look done" | the `turn_failed` outbound kind + `RearmFailedSchedule` re-arm path |
| Bounded retry then give up loudly | the runner's `maxTransientAttempts` + give-up notice |
| Operational record to inspect a run | `internal/eventlog` (host-writes, agent-reads-only) |
| Per-group identity/model/skills as data | `internal/agentspec` (Model / Harness / Context) |
| Strong isolation of agent work | the Podman per-agent-group container + single-writer-per-file boundary |
| Deterministic + adversarial gating | the pre-commit hook (computational) + adversarial review pattern (inferential), see `context-and-guardrails.md` §3 |
| Owner-only control commands | the router's owner-gated `/plugin`, `/schedule` |

A goal run is those primitives sequenced by a new host-side loop. Nothing here proposes a
new host<->container channel; the file boundary and its frozen rules are untouched (a task
run is still just inbound prompts in and outbound results out, per session).

## 3. Shape of a goal run

```
/goal "<broad goal>"                       (owner, from a channel)
   |
   v  SCOUT          host asks for the few decisions the goal leaves open
   |                 (recommends an answer); owner confirms
   v  PLAN           a planning turn drafts tasks (a list, later a DAG);
   |                 owner APPROVES the plan before anything runs   [GATE]
   v  per task:
   |    checkpoint   host records a known-good point (see §6)
   |    run          enqueue the task prompt into the run's session; runner executes
   |    VERIFY       host runs the task's check command(s) OUTSIDE the runner   [GATE]
   |    advance      pass -> next task; fail -> revert to checkpoint, retry with context;
   |                 stuck -> mark blocked, ask the owner                       [GATE]
   v  DONE           host reports the run: what ran, what was verified, the trail
```

The owner is in the loop at three gates (plan approval, a failed/blocked task, and any
step the plan marked "needs a human"), and nowhere else. Between gates it runs
unattended. This is the "stay in the loop on architecture, let the agent fill in" stance
from the design philosophy, applied to a whole run.

## 4. State: a goal run is host-owned, like the scheduler's

A goal run is HOST state in the central DB, never agent-writable (the agent cannot be
trusted to record whether its own work passed). Sketch of the tables, mirroring
`scheduled_tasks`:

```sql
CREATE TABLE goal_runs (
    id             TEXT PRIMARY KEY,        -- uuid
    owner_user_id  INTEGER NOT NULL,        -- who started it (authorization + listing)
    agent_group_id INTEGER NOT NULL,        -- where it runs
    session_key    TEXT NOT NULL,           -- the run's own session (fresh context)
    channel        TEXT NOT NULL,           -- where gate questions + the final report go
    chat_id        TEXT NOT NULL,
    goal           TEXT NOT NULL,           -- the owner's broad goal text
    state          TEXT NOT NULL,           -- scouting|planning|awaiting_approval|
                                            --   executing|blocked|done|cancelled|failed
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (owner_user_id)  REFERENCES users(id),
    FOREIGN KEY (agent_group_id) REFERENCES agent_groups(id)
);

CREATE TABLE goal_tasks (
    id            TEXT PRIMARY KEY,
    run_id        TEXT NOT NULL REFERENCES goal_runs(id) ON DELETE CASCADE,
    seq           INTEGER NOT NULL,         -- order within the run (DAG: + a deps column later)
    prompt        TEXT NOT NULL,            -- the instruction enqueued for this task
    verify_cmd    TEXT,                     -- the check run OUTSIDE the runner (may be empty)
    success_when  TEXT,                     -- human-readable pass criterion (for the report)
    status        TEXT NOT NULL,            -- pending|running|verifying|done|blocked|failed
    checkpoint    TEXT,                     -- known-good marker to revert to (see §6)
    attempts      INTEGER NOT NULL DEFAULT 0,
    block_reason  TEXT
);
```

State transitions follow the scheduler's hard-won discipline: stamp progress only when the
work is durably handed off, and a step that could not even be handed off re-fires rather
than being marked done. Writes are atomic; the run survives a host restart and the sweep
resumes it (it already recovers runners for sessions with pending work).

The per-task `prompt` is enqueued with a new inbound `source` of `goal:<run>:<task>`, the
same mechanism the scheduler uses (`task:<id>`) so a terminally-failed task is named in
the give-up notice and routed back to the run loop instead of lost.

## 5. Verification outside the runner (the gate it cannot fake)

This is the most important borrowed idea and it fits goclaw's existing stance exactly:
the agent must not be the one who certifies its own work passed.

- Each `goal_task` carries a `verify_cmd` (e.g. `go test ./...`, a build, a lint, a
  smoke test). After the runner reports a task complete, the HOST runs `verify_cmd` and
  the task advances only on a zero exit. A green self-report with a red verify is a
  failure.
- This is the **computational sensor** half of the phase gate in
  `context-and-guardrails.md` §3, applied at the task boundary. The **inferential** half
  (an adversarial review turn judging the diff) can be a second, advisory gate for tasks
  where a script cannot settle correctness.
- Fail-closed: a `verify_cmd` that cannot run (missing, errored, timed out) is NOT a
  pass; it is unknown, which blocks the task and asks the owner. Same rule as the rest of
  goclaw's security-sensitive code.

**Where does `verify_cmd` run? This is the one real design tension (OPEN, §9 q1).** goclaw's
whole containment win is that agent work happens inside the Podman container and the host
never executes agent-influenced content. If `verify_cmd` is agent-authored and the host
runs it on the host, that is a host-side execution of agent-influenced input, the exact
thing the boundary exists to prevent. Options, fail-closed by construction:

- **(a) Run verify in a SEPARATE, throwaway container** (not the runner's), from a clean
  checkout, with no credentials. This preserves "outside the agent that did the work"
  without giving anything host execution. Heaviest but safest; reuses the plugin-build
  sandbox pattern (untrusted source already compiles in a throwaway container, never on
  the host).
- **(b) Operator-allowlisted verify commands only.** The owner approves the check command
  per agent group (like the mount allowlist); the agent cannot introduce an arbitrary
  command to be host-run. Lighter; the safety rests on the allowlist being real.
- **(c) Verify inside a fresh runner turn in a NEW session**, so it is a different context
  than the one that did the work (cannot "remember" how it cheated) but still contained.
  Weakest separation (same container image, same trust tier) but no host execution at all.

Lean (a) or (b); never "host runs an agent-authored command." Whatever the choice, it must
not become a host<->container channel or a host-side exec of agent output.

## 6. Checkpoints and recovery

Each task runs from a known-good point so a bad task can be reverted and retried with the
failure as context, rather than leaving the workspace in a confused state.

- For code work the natural checkpoint is a git commit/branch in the work tree (goclaw's
  coding skill already mandates branch-not-main; a task checkpoint is a commit before the
  task and a reset-to-it on failure). The checkpoint marker lives in `goal_tasks.checkpoint`.
- For vault work the checkpoint is the vault's own git history (the librarian already
  treats the vault as a git repo with an append-only log).
- Recovery on host restart reuses the sweep: a run in `executing` with a task `running`
  but no live runner is treated as crashed-mid-task and reset to retry that task, exactly
  the pattern the scheduler/runner already use for a turn that died in an outage.

Retry is BOUNDED (reuse the runner's `maxTransientAttempts` shape): after N failed
attempts on a task, stop, mark the run `blocked`, and ask the owner with the evidence.
Better a visible block than an infinite retry loop (the lesson from the give-up fix).

## 7. The owner control surface

Task work is owner-only (the blast radius is large), gated like `/plugin` and `/schedule`:

```
/goal "<text>"          start a run (enters SCOUT)
/goal_list              runs and their state
/goal_status <id>       the plan, per-task status, what's blocking
/goal_approve <id>      approve the drafted plan (the PLAN gate)
/goal_answer <id> ...   answer a scout question or a blocked-task question
/goal_cancel <id>       stop a run (leaves checkpoints intact)
```

Gate questions and the final report are delivered through the normal outbound path to the
run's origin chat. A scheduled task could even START a goal run (the schedulers and a task
system share the enqueue primitive), but that composition is out of scope here.

## 8. What this deliberately does NOT do

- **No new host<->container channel.** A task run is inbound prompts and outbound results
  per session, same boundary, same single-writer files. If any part of this ever seems to
  need the agent to write host state or call the host live, stop: that crosses the line
  this whole design is built to respect.
- **No host execution of agent-authored commands.** See §5; verification runs contained or
  allowlisted, never as a host-side exec of agent output. This is the single biggest way a
  task system could quietly undo goclaw's containment, and the README of at least one peer
  project concedes it as a real hole. goclaw should not inherit it.
- **No autonomous control of the operator's machine.** Runs are owner-gated, bounded,
  checkpointed, and inspectable. The agent proposes; the owner approves the plan and every
  block.
- **No DAG on day one.** Start with a linear task list (seq). A dependency graph (so a
  blocked task doesn't stall independent ones) is a later refinement once the linear loop
  is proven, not a v1 requirement.

## 9. Open questions (NEED A DECISION)

1. **Where does `verify_cmd` run?** (§5 a/b/c). The crux. Must be contained or allowlisted;
   never a host-side exec of agent output. Probably (a) throwaway container or (b) allowlist.
2. **Linear list vs DAG**, and when. Lean linear first; DAG when "one block stalls the run"
   actually bites.
3. **Where does task work live in the code?** A new `internal/taskrun` (host loop + state)
   plus DB migrations, paralleling `internal/scheduler`? It should reuse the scheduler's
   fire/re-arm primitives rather than copy them, factor the shared "enqueue a sourced
   prompt + ensure runner + handle terminal failure" path out of the scheduler.
4. **Multi-model (planner vs worker)?** A peer uses "one model drafts, another reviews."
   goclaw is single-runner-per-group today; cross-model orchestration is a big addition and
   probably a separate RFC. Default: one model, with the adversarial-review gate done as a
   second turn of the same model (still catches a lot).
5. **Checkpoint scope for non-git work.** Code and vault have git; a task that touches a
   mounted external dir with no VCS has no natural checkpoint. Restrict task work to
   git-backed work trees first?
6. **Concurrency.** One run per agent group at a time (simplest, avoids two runs fighting in
   one work tree), or parallel runs in separate sessions/work trees? Default: one at a time
   per group.
7. **Personality / spec interaction.** A goal run is still an agent-group turn, so it inherits
   the group's `agentspec` (model, invariants, skills) and any personality layer. Confirm a
   personality may color the run's chat but never bend a `must` mid-run (it cannot, by the
   tiering, but the plan/verify prompts should state it).

## 10. Recommendation

Build it as a host-side layer (`internal/taskrun` + a `/goal` command set + the two tables),
sequencing primitives goclaw already has, with the boundary and the "verify outside the
agent, contained" rules as hard constraints. Resolve §9 q1 (verify location) and q2/q3
(shape and code home) before writing code; q4-q7 can be settled during implementation.
Start linear, one run per group, git-backed work only, single model with an adversarial
second-turn review. The smallest version that is still honest, where the host owns the loop,
the state, and the gates, and the agent never certifies its own work, is the right first
cut.
