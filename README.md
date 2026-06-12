# goclaw

<p align="center">
  <img src="assets/goclaw.png" alt="goclaw" width="400">
</p>

A self-hosted personal-AI-agent host in Go + Podman: it routes chat messages
into per-agent-group containers where a Claude agent runs with OS-level
isolation, then delivers replies back. Inspired by NanoClaw; see
[`docs/nanoclaw-go-podman-brief.md`](docs/nanoclaw-go-podman-brief.md) for the
full design, [`docs/security.md`](docs/security.md) for the threat model, and
[`docs/channels.md`](docs/channels.md) for channel setup.

The full message loop runs end to end: a real per-agent-group Podman container
drives Claude (multi-turn, with `/reset` and `/compact`), reads and writes a
knowledge vault, can clone repos and open pull requests, and runs scheduled
vault maintenance. The host pieces (router, delivery, sweep, permissions, mount
validation) are real and tested. An optional built-in **credential proxy** keeps
the raw API tokens (Anthropic and GitHub) out of the agent container, injecting
them on the wire so the container only ever sees a placeholder (see
[Credential proxy](#credential-proxy)); without it, the container holds the
tokens directly as a pragmatic shortcut.

## Layout

```text
cmd/goclaw/           entry: config, DB init, start loops, signal handling
cmd/claude-runner/    REAL in-container runner: drives Claude via agent-sdk-go
cmd/stub-runner/      echo stand-in for the runner (dev/testing)
internal/config/      env + .env configuration
internal/db/          central DB + migrations, session DB pair, queue helpers
internal/channels/    ChannelAdapter interface + registry
internal/channels/telegram/  Telegram adapter (the v0 channel)
internal/channels/discord/   Discord adapter (gateway websocket)
internal/router/      resolution + access gate + approval flow → inbound.db (REAL)
internal/delivery/    outbound.db poll, delivery auth, adapter dispatch (REAL)
internal/sweep/       runner recovery + idle-runner GC (REAL); pins channel-hosting groups
internal/runtime/     Podman lifecycle (CLI shell-out) + mount builder + env
internal/mounts/      allowlist load + path validation (REAL, unit-tested)
internal/permissions/ roles, sender policy, access gate (REAL)
internal/credstore/   encrypted credential store (AES-256-GCM) for the proxy (REAL)
internal/credproxy/   host-side credential-injecting proxy (REAL)
internal/vault/       OneCLI credential-proxy wiring at spawn time (stub, alt path)
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
| `GOCLAW_DISCORD_TOKEN` | _(unset)_ | enables the Discord channel (needs the Message Content intent) |
| `GOCLAW_OWNER_DISCORD_ID` | _(unset)_ | your Discord user id, seeded as an owner identity |
| `GOCLAW_PODMAN_BIN` | `podman` | podman binary |
| `GOCLAW_SECRET_ENCRYPTION_KEY` | _(unset)_ | base64 32-byte key encrypting the credential store (see [Credential proxy](#credential-proxy)) |
| `GOCLAW_CREDPROXY_PORT` | `18080` | host port the credential proxy listens on |
| `GOCLAW_PROXY_CA_KEY` / `GOCLAW_PROXY_CA_CERT` | _(unset)_ | optional PEM override for the proxy CA; auto-generated under `{data_dir}/proxy/` if unset |

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

### Runner launch modes

How the per-agent-group container gets started depends on config:

- **Host-launched, lazy (default with `GOCLAW_LAUNCH_RUNNER=1`).** The host starts
  a group's container on the first message routed to it, and the sweep reaps it
  after an idle TTL. The first reply per group is therefore slower (cold start);
  later messages hit a warm container. This is the normal mode and keeps idle
  groups from holding resources.
- **Host-launched, eager + pinned (a group hosting a channel plugin).** A channel
  plugin (`kind: channel`, e.g. an IRC bridge) runs INSIDE the container and must be
  connected to its upstream whenever the host is up, not wait for a first message. So
  when channel plugins are installed, the host eagerly launches that group's container
  at startup AND pins it, so the sweep never reaps it as idle. The channel stays
  always-on. (See [`docs/channels-plugin-design.md`](docs/channels-plugin-design.md).)
- **Out of band (`GOCLAW_LAUNCH_RUNNER` unset).** The host does not manage
  containers; you run a runner yourself (`cmd/claude-runner`, or the echo
  `cmd/stub-runner`) against a session's mounts. Used for testing the host<->runner
  boundary without Podman orchestration.

### Real Claude runner

The echo stub above proves the plumbing. To get actual Claude replies, use the
Claude runner image instead. It drives the `claude` CLI via
[`shindakun/agent-sdk-go`](https://github.com/shindakun/agent-sdk-go), so the
image bundles Node + the CLI.

```sh
# 1. Build the Claude runner image:
podman build -f container/claude.Containerfile -t goclaw-claude:latest .

# 2. .env - point at the Claude image and provide auth. RECOMMENDED: store the
#    key in the credential proxy instead (see below) so it never enters the
#    container. The direct-env key shown here is the simpler fallback.
GOCLAW_LAUNCH_RUNNER=1
GOCLAW_RUNNER_IMAGE=goclaw-claude:latest
GOCLAW_ANTHROPIC_API_KEY=sk-ant-api03-...   # fallback; prefer the credential proxy

go run ./cmd/goclaw
```

> **Recommended: use the [Credential proxy](#credential-proxy).** It keeps the
> raw Anthropic key (and your GitHub token) out of the agent container entirely.
> The direct `GOCLAW_ANTHROPIC_API_KEY` / `GOCLAW_GITHUB_TOKEN` shown here and
> below are the simpler path, but they put the real tokens inside the container
> where a prompt-injected agent could read them. With the proxy, the container
> only ever holds the literal string `placeholder`.

**Auth: use an API key.** A standard Anthropic API key
(`GOCLAW_ANTHROPIC_API_KEY`, from console.anthropic.com) is long-lived and the
runner keeps working indefinitely. A Claude Code subscription token
(`GOCLAW_CLAUDE_CODE_OAUTH_TOKEN`) is also supported and bills against your
Claude plan, **but it expires in ~12h and the container cannot refresh it** (on
macOS the live token is in the Keychain, unreachable from the container) - so it
will eventually 401 and you'll have to re-extract it. Prefer the API key for
anything left running. If both are set, the API key wins. Either way, the
[Credential proxy](#credential-proxy) is the more secure way to provide it.

> ⚠️ **Terms caution:** a Claude Code subscription token is intended for
> interactive Claude Code use, not for driving an automated/headless agent like
> this. Using it to run goclaw unattended may violate the subscription's terms of
> service. Check the current Anthropic / Claude terms before relying on it; for
> automation, the standard API key (billed as API usage) is the intended path.

Now messaging the bot gets a real Claude answer. The host passes the credential
into the container (`CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY`); the runner
calls `claude.Query` per message and writes the result to outbound.

**Conversation is multi-turn** - the runner persists Claude's session id per
conversation (in the session's `outbound.db`) and resumes it each message, so
Claude remembers the thread. Two chat commands manage it, and the runner
auto-compacts when the context window fills:

- `/reset` - forget this conversation, start fresh.
- `/compact` - summarize history to shrink context, keep the thread.

> ⚠️ **Security note:** by default the runner container holds the raw API key
> (passed as `ANTHROPIC_API_KEY`), which means the agent process can read it. To
> keep the key out of the container, use the [Credential proxy](#credential-proxy)
> below.

### Credential proxy

A built-in TLS-intercepting proxy keeps raw API tokens out of the agent container
(brief §8). The host holds the tokens encrypted; the runner routes its HTTPS
through the proxy (`HTTPS_PROXY`) and trusts the proxy's CA. The proxy injects
the real token per request at the host boundary, so a prompt-injected agent that
runs `echo $ANTHROPIC_API_KEY` or `echo $GH_TOKEN` gets only the literal string
`placeholder`. It covers Anthropic and GitHub today.

Set up:

```sh
# 1. A 32-byte base64 key that encrypts the credential store (keep it out of the
#    data dir; .env is fine, or source it from elsewhere).
export GOCLAW_SECRET_ENCRYPTION_KEY="$(head -c 32 /dev/urandom | base64)"

# 2. Store the real tokens. Each is encrypted at rest (AES-256-GCM) in goclaw.db,
#    never written in plaintext. The proxy matches an outbound request by target
#    host and injects the matching token.
goclaw auth add anthropic https://api.anthropic.com sk-ant-api03-...
# GitHub needs two hosts: github.com for `git clone`, api.github.com for `gh`.
goclaw auth add github https://github.com      github_pat_...
goclaw auth add github https://api.github.com  github_pat_...

goclaw auth list                 # id, name, target, masked token
goclaw auth delete <id>          # remove by the id from `list`
# For refreshable OAuth2 credentials (Gmail etc.) use `add-oauth`, see below.
```

When a credential is stored (and the encryption key is set), the host starts the
proxy and launches runners with `HTTPS_PROXY` pointing at it, the CA mounted, and
**placeholder** values for `ANTHROPIC_API_KEY` / `GH_TOKEN` - so you do **not**
set `GOCLAW_ANTHROPIC_API_KEY` or `GOCLAW_GITHUB_TOKEN`. With no stored
credential, goclaw falls back to passing the raw tokens as before.

How it injects, per host:

- `api.anthropic.com` -> `x-api-key: <token>`
- `github.com` / `codeload.github.com` (git smart-HTTP) -> HTTP Basic
  `x-access-token:<token>` (the git endpoints reject Bearer)
- everything else, including `api.github.com` (the `gh`/API host) ->
  `Authorization: Bearer <token>`

Hosts with no stored credential are blind-tunneled: the proxy pipes the bytes
without decrypting, so that traffic stays end-to-end encrypted to its real
destination.

### OAuth2 credentials (Gmail and other "sign in with Google" APIs)

The tokens above are STATIC. Some upstreams (Gmail, Calendar, Drive, anything
behind "sign in with Google") use OAuth2: a long-lived **refresh token** held
host-side, from which short-lived access tokens are minted and refreshed on
demand. `goclaw auth add-oauth` stores the refresh token encrypted; the proxy
refreshes and injects `Authorization: Bearer` per request, exactly like a static
token. The refresh token and access tokens never enter the agent container.

One-time Google setup (you need a client id + secret):

1. console.cloud.google.com -> create or pick a project.
2. APIs & Services -> Library -> enable the API you want (e.g. "Gmail API").
3. APIs & Services -> OAuth consent screen: "External", add your own Google
   account as a **test user**, add the scope you need. For the reply-by-email
   Gmail channel use `https://www.googleapis.com/auth/gmail.modify` (read +
   send + mark-read/archive in one scope). `gmail.readonly` is read-ONLY and
   will 403 on send, so it suits only a digest/summary tool, not replies.
4. APIs & Services -> Credentials -> Create credentials -> OAuth client ID ->
   application type **Desktop app**. This yields the client id
   (`...apps.googleusercontent.com`) and client secret. "Desktop app" is what
   makes the `http://127.0.0.1` loopback redirect the command uses acceptable.

Then store it (needs `GOCLAW_SECRET_ENCRYPTION_KEY` set, as above). The provider
data (auth/token URLs, scopes, the refresh-forcing params) comes from the named
PLUGIN's `oauth:` block in its `plugin.yml`, so the command only needs the
plugin and your client id/secret:

```sh
# Opens the browser to the provider's consent screen, catches the code on a
# throwaway 127.0.0.1 server that lives only for this command, exchanges it,
# stores the refresh token encrypted. The provider facts come from the gmail
# plugin's oauth block; the command prints them and asks you to confirm first.
goclaw auth add-oauth \
  --plugin gmail \
  --client-id <id>.apps.googleusercontent.com \
  --client-secret <secret>

# Already have a refresh token (e.g. from the OAuth playground)? Skip the browser:
#   ... --refresh-token <rt>
# Headless host (no browser)? Print the URL and paste the code back:
#   ... --no-browser
# Override a provider field (self-hosted instance, extra scopes):
#   ... --scopes "<s1> <s2>"   --token-url <url>   --auth-url <url>
```

`goclaw auth list` then shows it as kind `oauth2-bearer` (the token is never
displayed). Adding a NEW OAuth service is a plugin-authoring task (declare an
`oauth:` block; see goclawkit's sdk-spec), with no change to goclaw. Gmail (and
any OAuth2 upstream) requires the credproxy to be ON, since delivery is
proxy-inject only.

Notes and caveats:

- The agent's tools trust the proxy CA via `NODE_EXTRA_CA_CERTS` (the `claude`
  CLI), `GIT_SSL_CAINFO` (git/gh), and `SSL_CERT_FILE` (curl, Go, python). The CA
  is auto-generated under `{data_dir}/proxy/` (or supplied via
  `GOCLAW_PROXY_CA_KEY` / `GOCLAW_PROXY_CA_CERT`).
- `gh` is given a placeholder `GH_TOKEN` so it considers itself logged in (it
  checks locally before any request); the real token is injected on the wire.
- At-rest caveat: the encryption key lives in the environment, not the data dir,
  so a stolen data dir or DB dump does not include it - but a full host compromise
  still exposes both. This matches the standard "encrypt at rest with a local key"
  model. The proxy CA private key is similarly host-local.

You can also run the Claude runner directly (uses your host's logged-in Claude
session or `ANTHROPIC_API_KEY`):

```sh
go run ./cmd/claude-runner -dir data/sessions/<agentGroupID> -once
#   -model              override the model
#   -system-prompt-file e.g. a group's CLAUDE.md
```

### Plugins (run sandboxed in the agent container)

goclaw can be extended with **plugins**: small compiled binaries that add tools
(and, later, channels) without rebuilding or restarting the host. A plugin lives in
`data/plugins/<name>/` (under the data dir, as installed runtime state) as a binary
plus a `plugin.yml`, and the host launches plugins by mounting that directory into
the agent container.

The security property is the point. A plugin is untrusted, downloaded-and-compiled
code, so it must NOT run on the host. goclaw mounts the plugins dir read-only into
the agent container and the in-container runner launches each plugin there. Untrusted
plugin code therefore runs inside the same sandbox as the agent: rootless, non-root
(`1000:1000`), no view of the host filesystem beyond explicit mounts, and it dies
with the container. A malicious plugin can reach only what the container can, never
your host or its credentials. The host never executes a plugin binary.

The agent calls a plugin's tools as local in-container tools, and a user can invoke
a plugin's slash command directly (e.g. `/roll 2d6`, which dispatches to the plugin
with no model turn). See [`docs/plugins-design.md`](docs/plugins-design.md) for the
full design, the wire protocol, and the threat model; the reference plugin is a
dice roller built with the [goclawkit](https://github.com/shindakun/goclawkit) SDK.

### Development environment + GitHub

The Claude runner image is a real dev environment: git, the GitHub CLI (`gh`),
build-essential, Python 3, Go, ripgrep, jq, and more, so the agent can clone,
build, script, and open pull requests. To enable private clones and PRs, provide
a GitHub token.

> **Recommended: put the GitHub token in the [Credential proxy](#credential-proxy),
> not `GOCLAW_GITHUB_TOKEN`.** The proxy injects it on the wire so the container
> only sees a placeholder, whereas `GOCLAW_GITHUB_TOKEN` passes the raw token into
> the container as `GH_TOKEN` where the agent can read it. `git` and `gh` both work
> through the proxy (verified). Use a fine-grained PAT scoped to the repos you want
> the agent to reach.

The direct-env fallback:

```sh
# .env (simpler, but the raw token enters the container - prefer the proxy above)
GOCLAW_GITHUB_TOKEN=github_pat_...   # passed in as GH_TOKEN
GOCLAW_GIT_USER_NAME=Your Name       # commit author (defaults work)
GOCLAW_GIT_USER_EMAIL=you@example.com
```

Either way, "clone X, make this change, open a PR" works end to end: the agent
clones in `/work`, branches, commits (using your git identity), and runs
`gh pr create`. For a repo you don't own, `gh repo fork` pushes the branch to
your fork and opens the PR against upstream. Without any token, the agent can
still clone public repos but cannot push or open PRs.

### Discord channel

Discord is a second channel on the same `ChannelAdapter` interface (text in/out
plus a typing indicator, like Telegram). Enable it with a bot token and your
Discord user id:

```sh
# .env
GOCLAW_DISCORD_TOKEN=...        # the bot token (no "Bot " prefix)
GOCLAW_OWNER_DISCORD_ID=...     # your numeric Discord user id
```

The bot needs the **Message Content Intent** enabled and must be invited to your
server. See [`docs/channels.md`](docs/channels.md) for the full step-by-step
(including the "installation type not supported" invite fix and how to find your
user id). The same owner user can hold both a Telegram and a Discord identity, so
you can reach the agent from either; scheduled vault maintenance posts its summary
to the Telegram owner if set, otherwise the Discord owner's DM.

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

**Updating the vault after a goclaw upgrade.** `goclaw vault init` only ever
_fills in missing_ files, so it never refreshes the agent's operating-contract
files in an EXISTING vault. When a goclaw upgrade improves those (the librarian
skill, the vault prompt), pull them into your live vault with:

```sh
goclaw vault sync                  # ~/Vault (or: goclaw vault sync /path)
goclaw vault sync --dry-run        # show what WOULD change, write nothing
```

`sync` touches ONLY goclaw's own rulebook, an explicit allowlist:

- `.claude/skills/librarian/SKILL.md` (the librarian discipline)
- `CLAUDE.md` (the vault's system prompt)

It NEVER touches your content: `index.md`, `log.md`, `CRITICAL_FACTS.md`, and
every note under `wiki/` are left exactly as they are. Before replacing a changed
contract file it writes a `<file>.bak` next to it, so a sync is non-destructive
and reversible. Run it after upgrading goclaw (and re-run after editing the
templates if you maintain your own).

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
Done: the real Go runner on [`shindakun/agent-sdk-go`](https://github.com/shindakun/agent-sdk-go)
(multi-turn, `/reset`, `/compact`, auto-compact); **Telegram and Discord** channels
on the same `ChannelAdapter` interface; the knowledge vault mount with scheduled
maintenance; the GitHub dev environment (clone + open PRs); container teardown /
idle-runner GC (`internal/sweep`); the
[Credential proxy](#credential-proxy) that keeps the raw Anthropic and GitHub
tokens out of the agent container (verified live), with plugin-declared OAuth2
(Gmail) on top; a central slash-command registry with `/commands`;
[Plugins](#plugins-run-sandboxed-in-the-agent-container) with the full `/plugin`
command set (add/list/check/update/remove), build-on-install, and the `roll`
reference plugin; channel plugins on the same boundary (a relay socket per
channel), which activate live on `/plugin add` without a host restart;
user-definable scheduled tasks (`/schedule`); and a host-owned operational event
log (`internal/eventlog`).

Next:

1. More built-in channels (e.g. Slack) on the same `ChannelAdapter` interface
   (brief §7), or as channel plugins.
2. Outbound attachments end to end: inbound attachments are resolved (Telegram
   `GetFile`) and their labels sanitized against prompt injection, but sending a
   file OUT needs a boundary change first (an attachment column on `outbound.db`
   plus a runner-side convention for the agent to emit a file).
3. Validated extra mounts on the runner via the allowlist (brief §8).
4. More credential-proxy hosts as needed (the store and per-host injection are
   generic; add a host's auth scheme to `injectAuth` if it is neither x-api-key
   nor Bearer nor git-Basic).
