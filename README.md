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
internal/vaultinit/   `goclaw vault init` installer + embedded vault template (brief §11)
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

The full loop round-trips through a Podman container, one per agent group,
launched by the host on enqueue. For real Claude replies, jump to
[Real Claude runner](#real-claude-runner). The steps below use the **echo stub**
(`cmd/stub-runner`, no Claude/API key) to verify the plumbing first.

**1. Build the echo-stub image** (from the repo root):

```sh
podman build -f container/runner.Containerfile -t goclaw-runner:latest .
```

**2. Run the host with runner launch enabled**, pointed at the stub image (the
default is the Claude runner):

```sh
# .env
TELEGRAM_BOT_TOKEN=...                    # from @BotFather
GOCLAW_OWNER_TELEGRAM_ID=...              # your numeric Telegram id
GOCLAW_AUTO_WIRE_OWNER=1                  # first-run convenience
GOCLAW_LAUNCH_RUNNER=1                    # host launches the runner container
GOCLAW_RUNNER_IMAGE=goclaw-runner:latest  # the echo stub (omit for real Claude)

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

# 2. .env - point at the Claude image and provide auth:
GOCLAW_LAUNCH_RUNNER=1
GOCLAW_RUNNER_IMAGE=goclaw-claude:latest
GOCLAW_ANTHROPIC_API_KEY=sk-ant-api03-...   # recommended: long-lived

go run ./cmd/goclaw
```

**Auth: use an API key.** A standard Anthropic API key
(`GOCLAW_ANTHROPIC_API_KEY`, from console.anthropic.com) is long-lived and the
runner keeps working indefinitely - this is what NanoClaw uses too. A Claude Code
subscription token (`GOCLAW_CLAUDE_CODE_OAUTH_TOKEN`) is also supported and bills
against your Claude plan, **but it expires in ~12h and the container cannot
refresh it** (on macOS the live token is in the Keychain, unreachable from the
container) - so it will eventually 401 and you'll have to re-extract it. Prefer
the API key for anything left running. If both are set, the API key wins.

Now messaging the bot gets a real Claude answer. The host passes the credential
into the container (`CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY`); the runner
calls `claude.Query` per message and writes the result to outbound.

**Conversation is multi-turn** - the runner persists Claude's session id per
conversation (in the session's `outbound.db`) and resumes it each message, so
Claude remembers the thread. Two chat commands manage it, and the runner
auto-compacts when the context window fills:

- `/reset` - forget this conversation, start fresh.
- `/compact` - summarize history to shrink context, keep the thread.

> ⚠️ **Security note:** passing a credential into the container is the pragmatic
> shortcut. NanoClaw's design (brief §8) routes container traffic through a
> credential _vault_ proxy so the raw key never enters the container - that's the
> `internal/vault` stub, not yet wired. Until then, the runner container holds a
> usable credential.

You can also run the Claude runner directly (uses your host's logged-in Claude
session or `ANTHROPIC_API_KEY`):

```sh
go run ./cmd/claude-runner -dir data/sessions/<agentGroupID> -once
#   -model              override the model
#   -system-prompt-file e.g. a group's CLAUDE.md
```

### Development environment + GitHub

The Claude runner image is a real dev environment: git, the GitHub CLI (`gh`),
build-essential, Python 3, Go, ripgrep, jq, and more, so the agent can clone,
build, script, and open pull requests. Set a GitHub token to enable private
clones and PRs:

```sh
# .env
GOCLAW_GITHUB_TOKEN=github_pat_...   # passed in as GH_TOKEN
GOCLAW_GIT_USER_NAME=Your Name       # commit author (defaults work)
GOCLAW_GIT_USER_EMAIL=you@example.com
```

Then "clone X, make this change, open a PR" works end to end: the agent clones in
`/work`, branches, commits (using your git identity), and runs `gh pr create`.
For a repo you don't own, `gh repo fork` pushes the branch to your fork and opens
the PR against upstream. Without a token, the agent can still clone public repos
but cannot push or open PRs.

### Knowledge vault

Give the agent a persistent, Obsidian-style Markdown vault it reads and writes
(brief §11). Install one from the embedded template, then point goclaw at it:

```sh
goclaw vault init              # installs to ~/Vault (or: goclaw vault init /path)

# .env
GOCLAW_VAULT_DIR=~/Vault
```

The host mounts the vault read-write at `/vault` in the agent-group container.
The runner reads `/vault/CLAUDE.md` as its system prompt (so the agent behaves as
the vault's librarian) and runs with edits auto-approved, since the container is
the sandbox. The agent then maintains the vault per the schema: typed notes,
frontmatter, an `index.md` catalog, and an append-only `log.md` audit trail. You
browse and edit the same folder in Obsidian; git tracks every change.

**Scheduled upkeep** runs automatically when a vault and an owner are configured
(brief §11.5): a morning day-note pass, a nightly reconcile + synthesize, and a
weekly lint. Each runs the agent against the vault and posts a one-line summary
to the owner's chat. The agent commits its changes with git, so set
`GOCLAW_GIT_USER_NAME` / `GOCLAW_GIT_USER_EMAIL` to your identity (defaults work).

### Without container launch

Leave `GOCLAW_LAUNCH_RUNNER` unset to start a runner out of band instead -
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
