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
cmd/stub-runner/      minimal stand-in for the in-container runner (echoes inbound→outbound)
internal/config/      env + .env configuration
internal/db/          central DB + migrations, session DB pair, queue helpers
internal/channels/    ChannelAdapter interface + registry
internal/channels/telegram/  Telegram adapter (the v0 channel)
internal/router/      entity resolution + access gate → enqueue to inbound.db (REAL)
internal/delivery/    outbound.db poll, delivery auth, adapter dispatch (REAL)
internal/sweep/       60s ticker: stale, due-wake, recurrence (stub)
internal/runtime/     Podman lifecycle (CLI shell-out) + mount builder
internal/mounts/      allowlist load + path validation (REAL, unit-tested)
internal/permissions/ roles, sender policy, access gate (REAL)
internal/vault/       OneCLI credential-proxy wiring at spawn time (stub)
internal/vaultlock/   flock single-writer guard for the shared vault
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

Message the bot and you get `echo: <your text>` back — the host writes inbound,
launches the container (mounting the session dir at `/session`, `--user
1000:1000`, `--init`, `:Z`), the runner echoes to outbound, and delivery sends
the reply.

### Without container launch

Leave `GOCLAW_LAUNCH_RUNNER` unset to start the runner out of band instead —
useful for development without rebuilding the image:

```sh
go run ./cmd/stub-runner -dir data/v2-sessions/<agentGroupID>/<sessionKey>
#   -once      process the current backlog and exit
#   -interval  poll cadence (default 500ms)
```

`internal/delivery` has an on-disk round-trip test (`TestRoundTrip`), and
`internal/runtime` tests the launch argv + idempotent skip against a fake podman.

## Status / next steps

The full message loop works end to end through a real container. Next:

1. Replace the stub runner with the real agent-runner (Option A TS, or
   Option A′ Go on `shindakun/agent-sdk-go`; brief §4).
2. Per-agent-group container model (v0 runs one container per session).
3. Credential vault proxy + validated extra mounts on the runner (brief §8).
