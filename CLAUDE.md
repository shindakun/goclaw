# goclaw

A self-hosted Claude agent reachable over chat channels (Telegram, Discord). A
single Go host process routes messages from a channel to a per-agent-group Podman
container running Claude, then delivers the reply back. Go 1.26, module
`github.com/shindakun/goclaw`.

See `docs/nanoclaw-go-podman-brief.md` for the full design, `docs/security.md` for
the threat model, and `docs/channels.md` for channel setup.

## The message boundary: one SQLite pair PER CONVERSATION, single-writer-per-file

The host and the agent container exchange MESSAGES through a pair of SQLite files,
and there is one such pair **per session (= per conversation)**, NOT two databases
for the whole system. On disk: `data/sessions/<agentGroupID>/<sessionKey>/{inbound,
outbound}.db`, where `sessionKey` is the conversation (e.g. `telegram:6306189728`).
Three conversations means three pairs; a fourth Telegram chat adds a fourth. The
"two databases" shorthand has always meant "two files per conversation", not a
global count, this is NanoClaw's per-session model (see the brief), ported faithfully.

Each pair has exactly one writer per file:

- `inbound.db`  written by the HOST, read by the container.
- `outbound.db` written by the CONTAINER, read by the host (opened read-only).

The host opens `outbound.db` read-only so the single-writer-per-file rule is
enforced by the driver, not just promised. Before touching anything that crosses
this boundary, keep that rule intact: a second writer to either file is a
corruption/lost-write bug (we have hit it). The delivery ledger lives in
`inbound.db`; the host never writes `outbound.db`.

**Caveat on the old "no IPC, no socket" framing.** The original slogan ("the host
and container talk ONLY through files, no socket") is no longer literally true: the
channel-plugin relay (`docs/channels-plugin-design.md`) runs a real TCP/Unix socket
across the same host<->container line. "No networking" was never the actual
principle, it was a side effect. The load-bearing invariant is **single-writer-per-
cross-mount-file** (SQLite over virtiofs corrupts under two writers; see the brief
§5.1 note). Moving the agent path to a socket was evaluated and REJECTED on
containment grounds (the file boundary is pull-only: the untrusted agent gets no
live channel to the host); the file boundary stays and the cross-mount rules are
frozen. The "two mechanisms" shape (files for the agent, socket for channel
plugins) is deliberate defense-in-depth by trust level. See the brief §5.1 note.

## Layout

- `cmd/goclaw`        the host: routing, delivery, sweep, container lifecycle, channels.
- `cmd/claude-runner` the real in-container runner (drives Claude via agent-sdk-go).
- `cmd/stub-runner`   echo stand-in for testing the boundary without Claude/an API key.
- `internal/channels` `ChannelAdapter` interface + registry; `telegram/` and `discord/` adapters.
- `internal/router`   resolves user → messaging group → agent group → session; the access gate is here.
- `internal/delivery` polls `outbound.db`, authorizes, sends via the adapter.
- `internal/outscan` deterministic outbound content scanner (secret-shape + exact-needle) run send-side as defense-in-depth.
- `internal/sweep`    periodic runner recovery + scheduled wakeups.
- `internal/runtime`  per-agent-group Podman lifecycle (CLI shell-out), mounts, env injection, prompt composition.
- `internal/agentspec` host-side declarative agent-group spec (Model/Harness/Context); runtime + runner render it. NOT I/O.
- `internal/db`       central DB + migrations, the per-session DB pair, the queues.
- `internal/permissions` pure access-control logic (roles, scope, unknown-sender policy). Fail-closed.
- `internal/mounts`   external mount allowlist + validation (most security-critical: symlink resolution, `..`/colon rejection).
- `internal/credproxy` + `internal/credstore` bundled TLS-intercepting credential proxy + encrypted token store.
- `internal/vaultinit` `goclaw vault init` installer + the embedded vault template.
- `internal/maintenance` scheduled vault upkeep jobs.
- `internal/config`   env/.env config loading.

Note: `container/CLAUDE.md` and `internal/vaultinit/template/CLAUDE.md` are PROMPTS
for the agent inside the container, not instructions for working on this repo. This
file (the repo root `CLAUDE.md`) is the one for development.

## Build, run, test

```sh
go build ./...
go test ./...                 # whole suite
go test -race ./...           # concurrency-touching packages exist; use -race when relevant
go run ./cmd/goclaw           # run the host (reads .env; see .env.example)
```

Install the pre-commit hook once after cloning: `./.githooks/install.sh`. It gates
every commit on gofmt + go vet + `go build ./...` + `go test ./...`, so a tree that
does not compile or whose tests fail cannot be committed (a broken HEAD has slipped
through before). Live container tests are gated behind `GOCLAW_LIVE_INSTALL` and
skip in the hook. Bypass only in a real emergency with `git commit --no-verify`.

Container images (only when launching real runners):

```sh
podman build -f container/claude.Containerfile -t goclaw-claude:latest .   # real Claude runner
podman build -f container/runner.Containerfile -t goclaw-runner:latest .   # echo stub
```

If a change is baked into a container image, rebuilding is not enough: verify the
rebuilt image actually contains the change before claiming success.

## Conventions

- Write tests with the code, not after. New behavior gets a test in the same change;
  a bug fix gets a test that fails before the fix. Prefer table-driven tests.
- Tests must actually assert behavior. A test that passes whether or not the code is
  correct is worse than no test (it gives false confidence). If unsure a test has
  teeth, mutate the code and confirm the test fails.
- Run `go test ./...` before reporting work done. State failures with their output;
  do not claim green without running it.
- Keep packages small and single-purpose; match the surrounding style (naming,
  comment density, error wrapping with `fmt.Errorf("context: %w", err)`).
- Security code (permissions, mounts, delivery authorization) fails closed: an
  unknown or malformed input is denied, not allowed. Preserve that when editing.
- Secrets (`.env`: tokens, API keys, PATs) are gitignored and never printed or
  committed. The credential proxy keeps real tokens on the host; the container holds
  placeholders.

## Gotchas hit before (so you do not rediscover them)

- A new conversation is DROPPED until it is wired to an agent group ("no wiring for
  conversation"). `GOCLAW_AUTO_WIRE_OWNER=1` wires the owner's first message.
- The runner container launches lazily on the first message after host start, so the
  first reply is slower; every later message hits a warm container. This is intentional.
- `git`/`libcurl` read the LOWERCASE `https_proxy`; set both cases when wiring the proxy.
- GitHub git smart-HTTP needs HTTP Basic `x-access-token:<token>`, not Bearer; the
  api.github.com REST endpoint uses Bearer. Inject auth per host.
- Migrations are tracked by version number, not content hash, so editing a comment in
  an already-applied migration is safe (it will not re-run).
