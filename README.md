# goclaw

A self-hosted personal-AI-agent host in Go + Podman: it routes chat messages
into per-agent-group containers where a Claude agent runs with OS-level
isolation, then delivers replies back. Inspired by NanoClaw; see
[`nanoclaw-go-podman-brief.md`](nanoclaw-go-podman-brief.md) for the full design.

This is a **scaffold**: the package layout and boundaries from §10 of the brief
are in place and the project compiles, but most host logic is stubbed with
`TODO`s. The security-critical mount validation (`internal/mounts`) has real
logic and tests.

## Layout

```text
cmd/goclaw/           entry: config, DB init, start loops, signal handling
cmd/claude-runner/    REAL in-container runner: drives Claude via agent-sdk-go
cmd/stub-runner/      echo stand-in for the runner (dev/testing)
internal/config/      env + .env configuration
internal/db/          central DB + migrations, session DB pair, queue helpers
internal/channels/    ChannelAdapter interface + registry
internal/channels/telegram/  Telegram adapter (the v0 channel)
internal/router/      resolution + access gate + approval flow → inbound.db (REAL)
internal/delivery/    outbound.db poll, delivery auth, adapter dispatch (REAL)
internal/sweep/       runner recovery + idle-runner GC (REAL)
internal/runtime/     Podman lifecycle (CLI shell-out) + mount builder + env
internal/mounts/      allowlist load + path validation (REAL, unit-tested)
internal/permissions/ roles, sender policy, access gate (REAL)
internal/vault/       OneCLI credential-proxy wiring at spawn time (stub)
internal/vaultlock/   flock single-writer guard for the shared vault
container/            Containerfiles: runner (echo stub) + claude (real runner)
secondbrain-template/ optional second-brain vault starter (brief §11)
```

## Build & run

```sh
go build ./...
go test ./...

# Run the host (Telegram disabled unless a token is set):
export TELEGRAM_BOT_TOKEN=...        # from @BotFather
go run ./cmd/goclaw
```

Config via environment:

| Var | Default | Purpose |
| --- | --- | --- |
| `GOCLAW_DATA_DIR` | `data` | root for central + session DBs |
| `GOCLAW_MOUNT_ALLOWLIST` | `~/.config/goclaw/mount-allowlist.json` | external mount allowlist (fail-closed if absent) |
| `TELEGRAM_BOT_TOKEN` | _(unset)_ | enables the Telegram channel |
| `GOCLAW_PODMAN_BIN` | `podman` | podman binary |

## Running the loop end to end

The full loop round-trips through a Podman container. The runner image packages
`cmd/stub-runner` (an echo stand-in — no Claude yet); the host launches one
container per session on enqueue.

**1. Build the runner image** (from the repo root):

```sh
podman build -f container/runner.Containerfile -t goclaw-runner:latest .
```

**2. Run the host with runner launch enabled:**

```sh
# .env
TELEGRAM_BOT_TOKEN=...          # from @BotFather
GOCLAW_OWNER_TELEGRAM_ID=...    # your numeric Telegram id
GOCLAW_AUTO_WIRE_OWNER=1        # first-run convenience
GOCLAW_LAUNCH_RUNNER=1          # host launches the runner container

go run ./cmd/goclaw
```

Message the bot and you get `echo: <your text>` back. One container runs **per
agent group** (named `goclaw-<agentGroupID>`), mounting that group's sessions
directory at `/sessions` (`--user 1000:1000`, `--init`, `:Z`); the runner serves
every session subdir beneath it, echoing to outbound, and delivery sends the
reply.

Session storage on disk:

```text
data/sessions/<agentGroupID>/<sessionKey>/{inbound.db,outbound.db}
```

### Real Claude runner

The echo stub above proves the plumbing. To get actual Claude replies, use the
Claude runner image instead. It drives the `claude` CLI via
[`shindakun/agent-sdk-go`](https://github.com/shindakun/agent-sdk-go), so the
image bundles Node + the CLI.

```sh
# 1. Build the Claude runner image:
podman build -f container/claude.Containerfile -t goclaw-claude:latest .

# 2. .env — point at the Claude image and provide auth (pick one):
GOCLAW_LAUNCH_RUNNER=1
GOCLAW_RUNNER_IMAGE=goclaw-claude:latest
#   (a) Claude Code subscription:
GOCLAW_CLAUDE_CODE_OAUTH_TOKEN=...   # see .env.example for Keychain extraction
#   (b) or a standard API key:
GOCLAW_ANTHROPIC_API_KEY=sk-ant-api03-...

go run ./cmd/goclaw
```

Now messaging the bot gets a real Claude answer. The host passes the credential
into the container (`CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY`); the runner
calls `claude.Query` per message and writes the result to outbound.

**Conversation is multi-turn** — the runner persists Claude's session id per
conversation (in the session's `outbound.db`) and resumes it each message, so
Claude remembers the thread. Two chat commands manage it, and the runner
auto-compacts when the context window fills:

- `/reset` — forget this conversation, start fresh.
- `/compact` — summarize history to shrink context, keep the thread.

> ⚠️ **Security note:** passing a credential into the container is the pragmatic
> shortcut. NanoClaw's design (brief §8) routes container traffic through a
> credential _vault_ proxy so the raw key never enters the container — that's the
> `internal/vault` stub, not yet wired. Until then, the runner container holds a
> usable credential.

You can also run the Claude runner directly (uses your host's logged-in Claude
session or `ANTHROPIC_API_KEY`):

```sh
go run ./cmd/claude-runner -dir data/sessions/<agentGroupID> -once
#   -model              override the model
#   -system-prompt-file e.g. a group's CLAUDE.md
```

### Without container launch

Leave `GOCLAW_LAUNCH_RUNNER` unset to start a runner out of band instead —
useful for development without rebuilding the image. Point it at an agent
group's sessions directory:

```sh
go run ./cmd/stub-runner -dir data/sessions/<agentGroupID>     # echo
go run ./cmd/claude-runner -dir data/sessions/<agentGroupID>   # real Claude
#   -once      process the current backlog and exit
#   -interval  poll cadence
```

`internal/delivery` has an on-disk round-trip test (`TestRoundTrip`), and
`internal/runtime` tests the launch argv + idempotent skip against a fake podman.

## Status / next steps

The full message loop works end to end through a real per-agent-group container.
Next:

1. Replace the stub runner with the real agent-runner (Option A TS, or
   Option A′ Go on `shindakun/agent-sdk-go`; brief §4).
2. Container teardown / GC of idle runners (sweep).
3. Credential vault proxy + validated extra mounts on the runner (brief §8).
