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

## Proving the boundary (no container yet)

The two-SQLite-file boundary round-trips end to end without a container, using
`cmd/stub-runner` as a stand-in for the real agent-runner. The host enqueues an
inbound message; the stub runner echoes it to outbound; delivery dispatches it.

```sh
# Point the stub runner at a session dir created by the host and let it echo:
go run ./cmd/stub-runner -dir data/v2-sessions/<agentGroupID>/<sessionKey>
#   -once      process the current backlog and exit
#   -interval  poll cadence (default 500ms)
```

`internal/delivery` has an on-disk round-trip test (`TestRoundTrip`):
host enqueues inbound → stub runner consumes + echoes → delivery sends via the
adapter, with origin-chat-allowed / non-origin-denied authorization (brief §9).

## Status / next steps

Inbound and outbound paths are real; the loop round-trips through the stub
runner. Next:

1. Container spawn from `internal/runtime`: run the stub runner (then the real
   Claude runner) inside a Podman container instead of on the host.
2. Container wake on enqueue (the `// TODO` in `router.enqueue`).
3. Replace the stub runner with the real agent-runner (Option A TS, or
   Option A′ Go on `shindakun/agent-sdk-go`; brief §4).
