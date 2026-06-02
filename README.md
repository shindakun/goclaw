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
internal/config/      env-based configuration
internal/db/          central DB + migrations (embedded SQL), session DB open
internal/channels/    ChannelAdapter interface + registry
internal/channels/telegram/  Telegram adapter (the v0 channel)
internal/router/      entity resolution + access gate → inbound.db (stub)
internal/delivery/    outbound.db poll, adapter dispatch, delivery auth (stub)
internal/sweep/       60s ticker: stale, due-wake, recurrence (stub)
internal/runtime/     Podman lifecycle (CLI shell-out) + mount builder
internal/mounts/      allowlist load + path validation (REAL, unit-tested)
internal/permissions/ roles, sender policy, access gate
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

## Status / next steps

Roughly Phase 0–1 of the brief's plan. Next:

1. Inbound/outbound session schema + the router write path.
2. Container spawn from `internal/runtime` round-tripping one Telegram message.
3. Delivery authorization against `agent_destinations`.
