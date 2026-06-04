# Technical Brief: Reimplementing NanoClaw in Go + Podman

**Subject:** Porting the NanoClaw personal-AI-agent host from TypeScript/Node + Docker to Go + Podman
**Date:** May 2026
**Status:** Feasibility + design recommendation

---

## 1. TL;DR

NanoClaw is a single host process that routes messages from chat channels (Telegram, Slack, Discord, etc.) into **per-agent-group containers** where a Claude agent runs with OS-level isolation, then delivers replies back. The host↔container boundary is deliberately thin: **two SQLite files per session, each with exactly one writer.** No IPC, no sockets, no stdin piping.

That thin boundary is the single most important fact for this port. It means **you can rewrite the host in Go and swap Docker for Podman without touching the agent code that runs inside the container at all.** The two sides only ever talk through `inbound.db` / `outbound.db`.

**Recommended approach: a Go host on Podman, with the in-container runner ported to Go too - staged.**

- Rewrite the **host orchestrator** (routing, delivery, scheduling, DB, permissions, mount validation, container lifecycle, credential proxying) in Go.
- Run it on **Podman**, which is a *better* fit than Docker for NanoClaw's security thesis (rootless + daemonless by default).
- For the **in-container agent-runner**, you now have a real choice: a Go Agent SDK exists ([`shindakun/agent-sdk-go`](https://github.com/shindakun/agent-sdk-go)), so the runner can be Go *and* keep the full `claude`-CLI harness (loop, tools, Skills, subagents). Recommended path: **ship the Go host first against the existing TS runner** (proves the SQLite boundary), then **swap the runner to Go** once that port is exercised on real traffic - see §4.

This gets you Go's concurrency and single-binary deployment on the host that benefits most, Podman's stronger isolation posture, and a clean path to a single-language codebase without giving up the harness.

An optional **knowledge vault** - a shared, Obsidian-style Markdown vault that both you and the agent read and write - drops in cleanly on top of this. It's just a read-write mount plus a write-lock and some note-writing discipline; the Go host barely changes. See §11.

---

## 2. What NanoClaw actually is

A lightweight, self-hosted alternative to OpenClaw: ~15 source files, one Node process, <10 dependencies. It connects messaging channels to Claude agents, but instead of application-level permission checks it relies on **real Linux container isolation** - each agent group runs in its own container and can only see directories you explicitly mount. Built on Anthropic's Claude Agent SDK, MIT-licensed.

The design philosophy ("small enough to understand," "secure by isolation," "customization = code changes") matters for the port: the goal is not to add a framework layer, but to keep the Go version equally small and auditable.

---

## 3. Current architecture (the parts that matter for porting)

### 3.1 Data flow

```
messaging apps
   → host process (router)        # resolve sender, route, gate access
   → inbound.db                   # one writer: the host
   → container (Claude Agent SDK) # poll inbound, run Claude, write replies
   → outbound.db                  # one writer: the agent-runner
   → host process (delivery)      # poll outbound, deliver, run system actions
   → messaging apps
```

### 3.2 Entity model

Routing resolves a chain: **user → messaging group → agent group → session.** A central SQLite DB holds users, roles, agent groups, messaging groups, the wiring between them, and migrations. Each session gets isolated DBs at `data/v2-sessions/{agent_group_id}/{session_id}/`.

### 3.3 Key files (current TypeScript)

| File | Responsibility |
|---|---|
| `src/index.ts` | Entry point: DB init, channel adapters, delivery poll, sweep loop |
| `src/router.ts` | Inbound routing: messaging group → agent group → session → `inbound.db` |
| `src/delivery.ts` | Polls `outbound.db`, delivers via adapter, handles system actions |
| `src/host-sweep.ts` | 60-second sweep: stale detection, due-message wake, recurrence |
| `src/session-manager.ts` | Resolves sessions, opens `inbound.db` / `outbound.db` |
| `src/container-runner.ts` | Spawns per-agent-group containers, OneCLI credential injection |
| `src/db/` | Central DB and migrations |
| `src/channels/` | Channel adapter infra (adapters skill-installed per fork) |
| `src/providers/` | Host-side provider config (`claude` baked in) |
| `container/agent-runner/` | The thing inside the container: poll loop, MCP tools, provider abstraction |
| `container/CLAUDE.md`, `container/skills/` | Generic agent-first base prompt + skills, baked into the image (§11.0); the per-group `CLAUDE.md` is composed from these at launch |

Everything from `src/index.ts` down to `src/db/` is **host-side** and is the target of the Go rewrite. `container/agent-runner/` is the part that needs the Agent SDK; you can leave it on TS to start (Option A) or port it to Go on `shindakun/agent-sdk-go` (Option A′) - see §4. Either way it stays decoupled from the host across the SQLite boundary.

### 3.4 Security boundaries (must be preserved)

1. **Container isolation** - primary boundary; non-root execution as uid 1000; `tini` as PID 1 for signal handling.
2. **Mount security** - external allowlist at `~/.config/nanoclaw/mount-allowlist.json` (outside project root, never mounted). Symlink resolution before validation, rejection of `..` and absolute container paths, rejection of colons in container paths (prevents `-v` option injection), read-write gating.
3. **Permissions** - owner/admin/member roles; unknown-sender policy (`public` / `strict` / `request_approval`); per-wiring `sender_scope`; channel + sender approval flows.
4. **Session isolation** - each session sees only its own two DBs.
5. **Credential handling** - OneCLI Agent Vault injects credentials at request time; raw API keys never enter the container.
6. **Delivery authorization** - an agent may only deliver to its origin chat or to channels with an explicit `agent_destinations` row.

---

## 4. The central porting decision: the in-container runner

This is the crux, and it's worth being precise because it determines the whole design.

As of 2026, the relevant SDKs sit at three levels:

- **Anthropic Client SDK** - the Messages API client. **Go is officially supported** (`github.com/anthropics/anthropic-sdk-go`). With the client SDK, *you* implement the agent loop yourself: call `messages.create`, detect `stop_reason == "tool_use"`, execute the tool, feed back a `tool_result`, repeat. Using this means rebuilding the harness from scratch.
- **Claude Agent SDK** - the higher-level toolkit that runs that loop *for you*, with built-in bash/file/web tools, Skills, and subagents. Anthropic ships it for **TypeScript and Python**; it is what NanoClaw's `container/agent-runner/` currently uses. Critically, the Agent SDK does **not** reimplement the loop either - it **drives the user-installed `claude` Code CLI as a subprocess**, speaking newline-delimited `stream-json` over stdin/stdout plus a control protocol. The CLI owns the loop, tools, Skills, subagents, and context management; the SDK owns process lifecycle, framing, and dispatch.
- **A Go Agent SDK now exists.** [`github.com/shindakun/agent-sdk-go`](https://github.com/shindakun/agent-sdk-go) is a faithful, idiomatic Go port of the Claude Agent SDK that works the *same way* the official one does - it drives the `claude` CLI subprocess rather than reimplementing the loop. It reports static parity with upstream (123/123 public names, 45 options) verified against Claude Code CLI 2.1.159, with behavioral tests against the real binary, MIT-licensed. It provides `Query()` (single-turn), `NewClient()` (multi-turn full-duplex), streaming message iteration, in-process tools as SDK MCP servers, hooks, permission controls, and transcript/session reading. **This removes the gap the original version of this brief was built around.**
- **Claude Managed Agents (beta)** - a *hosted* REST API (`/v1/agents`, `/v1/environments`, `/v1/sessions`) that supports Go. But Anthropic runs the agent and the sandbox on **their** infrastructure. That directly contradicts NanoClaw's reason to exist ("agent runs in your container, on your machine, isolated by your OS"), so it is not a fit.

What changes with the Go Agent SDK: because it drives the same `claude` CLI, **you can write the in-container runner in Go and still keep the CLI's agent loop, built-in tools, Skills, and subagents** - none of which you'd get from hand-rolling on the Client SDK. The earlier trade-off ("single language *or* the full harness") is gone. One requirement carries over from the official SDK: the `claude` CLI binary must be present inside the container (installable via npm), so the container image still needs Node - the runner process itself is now Go.

So the realistic paths, re-scored:

| Option | What it means | Verdict |
|---|---|---|
| **A - TS hybrid** | Port the host to Go + Podman; keep `container/agent-runner/` on the TS Agent SDK, unchanged. The Go host only ever touches the two SQLite files and the container lifecycle. | **Solid, lowest-churn.** Ports the host (which benefits most from Go) while leaving a known-good runner untouched. The safe default if you want to ship the host port first and revisit the runner later. |
| **A′ - Full Go via the Go Agent SDK (now recommended)** | Port the host to Go + Podman *and* rewrite `container/agent-runner/` in Go on `shindakun/agent-sdk-go`. The runner still drives the `claude` CLI, so it keeps the loop, tools, Skills, and subagents. | **Recommended once the Go runner is proven on your workload.** Gets the single-language codebase NanoClaw's "small enough to understand" ethos prefers, with no loss of harness capability. Cost: depends on a third-party port and still needs the `claude` CLI (Node) in the image. |
| **B - Client-SDK, hand-built loop** | Reimplement the runner in Go on `anthropic-sdk-go`, hand-writing the tool loop and re-implementing bash/file/web tools. | **No longer attractive.** A′ gives you Go *and* the real harness; B gives you Go but throws the harness away. Only pick B to drop the `claude`-CLI/Node dependency entirely - a large, ongoing cost. |
| **C - Managed Agents in Go** | Use the hosted agent API. | **Rejected** - execution leaves the user's machine, defeating the isolation thesis. |

The thin SQLite boundary is what makes both A and A′ clean: the host writes a row to `inbound.db` and wakes the container; the runner (TS *or* Go) writes to `outbound.db`. Neither side cares what language the other is written in - which is also why you can **start on A and migrate the runner to A′ later** without touching the host. A pragmatic sequence: ship the Go host against the existing TS runner (proves the boundary), then swap in the Go runner once `shindakun/agent-sdk-go` is exercised against your real message traffic.

---

## 5. Host port map: TypeScript → Go

| Concern | Current (Node/TS) | Go replacement |
|---|---|---|
| Runtime model | Single event loop, async/await | Goroutines: one per session poll loop, one for delivery, one for the sweep ticker; coordinate with `context.Context` |
| SQLite | better-sqlite3 / Bun sqlite | `modernc.org/sqlite` (pure-Go, no cgo - keeps the static-binary story) or `mattn/go-sqlite3` (cgo, fastest). Enable WAL mode. |
| Central DB + migrations | `src/db/` | Same schema; migrations via embedded `.sql` + a tiny runner, or `golang-migrate`. Keep the schema identical so v2 data migrates 1:1. |
| Container lifecycle | `src/container-runner.ts` (Docker) | Podman bindings (`github.com/containers/podman/v5/pkg/bindings`) **or** shell out to the `podman` CLI. See §6. |
| Channel adapters | `src/channels/` (Chat SDK bridge) | Go interface `ChannelAdapter` with `Receive() <-chan InboundMsg` / `Send(OutboundMsg) error`. **Start with Telegram only** - pure-Go, low-risk; more channels drop in behind the same interface later. See §7. |
| Router | `src/router.ts` | Pure logic; ports directly. Entity resolution + access gate → write to `inbound.db`. |
| Delivery | `src/delivery.ts` | Goroutine polling `outbound.db`; dispatch via adapter; handle system actions + delivery authorization. |
| Sweep | `src/host-sweep.ts` | `time.Ticker` at 60s: stale detection, due-message wake, recurrence. |
| Process supervision | launchd / systemd unit | **Podman Quadlets** - generate native systemd units from a declarative `.container` file (replaces the `launchd/` dir cleanly on Linux). |
| Single static binary | N/A (needs Node + pnpm) | `go build` → one binary. Removes the Node/pnpm bootstrap from `nanoclaw.sh`. |

### 5.1 Concurrency: the real shift

Node's model is one event loop; concurrency is cooperative. Go gives you true parallel goroutines, which maps naturally onto NanoClaw's structure:

- One goroutine per active session watching `inbound.db` (or a single watcher fanning out).
- One delivery goroutine per channel, or a worker pool draining `outbound.db`.
- One sweep ticker.
- A top-level `context.Context` for graceful shutdown; cancel it and every loop unwinds.

The **single-writer-per-DB invariant must be preserved**. In Go this is easy to enforce: give each `*sql.DB` for a writable file a single owning goroutine, and never share the write handle. Set `db.SetMaxOpenConns(1)` on writer handles and use WAL so readers don't block the writer. This is arguably *safer* in Go than in Node, because you can make the ownership explicit per goroutine.

> **"One writer per file" is load-bearing for delivery, not just routing - and it is easy to violate by accident.** The temptation is to track delivery progress (a `pending → sending → delivered` status) by writing the status back into `outbound.db`. **Do not.** `outbound.db` is the *runner's* file; the host writing it makes two processes write one file across the podman VM mount, where page caches aren't coherent - the runner's next write resurrects the host's `delivered` row back to `pending` and the reply is sent twice. This is exactly how NanoClaw does it: the **host records delivery in a `delivered` table inside `inbound.db`** (the host-owned file) with an idempotent `INSERT OR IGNORE` keyed by the outbound message id, and **never writes `outbound.db` at all** (see NanoClaw `src/host-sweep.ts`: *"Never writes to outbound.db - preserves single-writer-per-file invariant"*). The delivery loop reads `outbound.db` (a 'pending' row there is only a *hint*, never authoritative), checks the ledger, sends, then writes the ledger. Three cross-mount rules make this work, all load-bearing: (1) `journal_mode=DELETE` - WAL's `-shm` doesn't refresh across the mount; (2) host opens-writes-closes per op - a long-lived handle freezes its page-cache view; (3) exactly one writer per file. Even with all three, virtiofs can still latch a torn snapshot, so the runner detects a corruption streak and exits for the host to respawn it with a fresh mount (NanoClaw `container/agent-runner/src/poll-loop.ts`). **Enforce rule (3) at the driver, not by convention:** the host should open `outbound.db` *read-only* (`mode=ro`), as NanoClaw does (`new Database(path, { readonly: true })`), so an accidental host write fails loudly instead of silently corrupting across the mount. A host opening a not-yet-created outbound.db read-only must treat its absence as "nothing pending" rather than an error (read-only mode can't create the file).

---

## 6. Runtime port: Docker → Podman

Podman is not just a drop-in Docker replacement here - it actively strengthens NanoClaw's security model.

### 6.1 Why Podman is a better fit

| Property | Docker | Podman | Relevance to NanoClaw |
|---|---|---|---|
| Daemon | Central root daemon | **Daemonless** (fork/exec per container) | Smaller attack surface; no privileged background service; aligns with "small enough to understand." |
| Default user | Root daemon spawns containers | **Rootless by default** via user namespaces | Container uid 1000 maps to an unprivileged *host* uid. The "non-root execution" boundary becomes a host-kernel guarantee, not just an in-container convention. |
| CLI/API compat | - | Largely Docker-CLI compatible; ships a Docker-compatible REST API | Eases the mechanical port. |
| systemd integration | Limited | **Quadlets** generate first-class systemd units | Replaces launchd/systemd scripting. |

### 6.2 Two ways to drive Podman from Go

1. **Bindings** (`github.com/containers/podman/v5/pkg/bindings`): typed Go API to a Podman service socket. Cleaner, testable, no string-building.
2. **CLI shell-out** (`exec.Command("podman", "run", ...)`): simplest, mirrors what `container-runner.ts` effectively does, easy to audit. Reasonable for a first cut given NanoClaw's "keep it readable" ethos.

Either works. Start with CLI shell-out for v0 to keep the diff small and auditable; move to bindings if you want stronger typing and to avoid the parsing of CLI output.

### 6.3 Mount handling - port the validation carefully

The mount-validation logic is security-critical and ports almost verbatim, with **Podman-specific additions**:

- Keep: symlink resolution before validation, reject `..` and absolute container paths, reject colons in container paths (the `-v` option-injection guard applies to `podman` exactly as to `docker`), RW gating.
- Add: Podman volume mounts on SELinux systems need a relabel suffix - `:Z` (private) or `:z` (shared). For NanoClaw's per-session isolation, **`:Z` is correct** (each container gets a private label). Bake this into the mount builder, and keep read-only mounts as `:ro,Z`.
- Rootless Podman uses UID/GID mapping; verify mounted host dirs are readable by the mapped host UID, or use `--userns=keep-id` when the agent must read files owned by the invoking user.

### 6.4 The "micro-VM / Apple Container" equivalents

NanoClaw offers optional stronger isolation (Docker Sandboxes micro-VMs; Apple Container on macOS). The Podman-native analogs:

- **Kata Containers** as the runtime → each container in a lightweight VM (the direct micro-VM analog).
- **gVisor** (`runsc`) as an OCI runtime under Podman → syscall-interception sandbox, lighter than a full VM.

Expose this the same way NanoClaw does: a per-agent-group runtime setting, defaulting to the standard rootless runtime (`crun`).

---

## 7. The communication layer (channel adapters)

This is the *other* half of the host the Go rewrite fully owns (alongside the container lifecycle): the bridge between the outside messaging world and the SQLite boundary. The container never touches a network socket to Telegram or Slack - **adapters are a pure host-side concern**, which is exactly why they port cleanly and why a per-channel feature gap (§7.4) never threatens the isolation model.

### 7.1 Where it sits in the data flow

Adapters bracket the two-DB boundary on the host side - inbound on the way in, outbound on the way out:

```
messaging app ──▶ adapter.Receive() ──▶ router ──▶ inbound.db
                                                       │
                                              (container runs Claude)
                                                       ▼
messaging app ◀── adapter.Send() ◀── delivery ◀── outbound.db
```

An adapter does **only** transport + identity normalization. It does *not* make routing or permission decisions - those belong to the router (§5) and the permissions layer (§9). This separation is what lets you add a channel without touching security code.

### 7.2 The `ChannelAdapter` interface

Every channel implements one small Go interface; the host treats them uniformly through a registry:

```go
type ChannelAdapter interface {
    // Name returns the channel id ("telegram", "slack", "discord", …).
    Name() string

    // Start connects (long-poll, websocket, or webhook) and streams
    // normalized inbound messages until ctx is cancelled.
    Start(ctx context.Context) (<-chan InboundMsg, error)

    // Send delivers one reply. Called by the delivery goroutine only.
    Send(ctx context.Context, out OutboundMsg) error
}

type InboundMsg struct {
    Channel   string            // "telegram"
    ChatID    string            // channel-native conversation id
    SenderID  string            // channel-native, stable user id
    Sender    string            // display name (best-effort)
    Text      string
    Attachments []Attachment    // files/images → feed §11 ingest
    Raw       json.RawMessage   // original payload, for debugging
    Timestamp time.Time
}

type OutboundMsg struct {
    Channel string
    ChatID  string
    Text    string
    Attachments []Attachment
    Action  *SystemAction       // optional: typing indicator, reaction, etc.
}
```

Two rules keep the boundary honest:

- **Normalize identity at the edge.** `SenderID`/`ChatID` are whatever the channel gives, but they must be *stable* (Telegram's numeric ids, Slack's `Uxxxx`). The router resolves these to NanoClaw users; the adapter must not invent its own.
- **One owning goroutine per adapter.** `Start` runs in its own goroutine and feeds a shared inbound channel; `Send` is only ever called from the single delivery goroutine. This preserves the single-writer discipline (§5.1) all the way out to the wire and means an adapter never needs its own locks.

### 7.3 Concurrency & lifecycle

- Each adapter's `Start` is launched under the top-level `context.Context`; cancel it and every connection unwinds for graceful shutdown.
- Inbound: a fan-in - all adapters write to one `<-chan InboundMsg` the router drains; or one router goroutine per channel if you prefer isolation.
- Outbound: the delivery goroutine reads `outbound.db`, looks up the target adapter in the registry by `Channel`, and calls `Send` - *after* the delivery-authorization check (§9), never before.
- **Reconnect/backoff** is the adapter's job: messaging APIs drop connections routinely, so each `Start` loop should reconnect with backoff rather than returning an error that kills the channel.
- **Outbound rate limits** are real (Telegram ~30 msg/s, Slack tiered, Discord per-route buckets). Give `Send` a per-channel limiter so a chatty agent can't get the channel banned.

### 7.4 Per-channel library recommendations

The point of the interface is that channel choice is a leaf decision. **Start with Telegram only** - it's the lowest-risk channel and the one the rest of this brief targets first. The others below take the same interface with no host changes when you're ready; all are low-risk and pure-Go:

| Channel | Recommended Go path | Transport |
| --- | --- | --- |
| **Telegram** (start here) | `github.com/go-telegram-bot-api/telegram-bot-api` (or `gotgbot`) | long-poll or webhook |
| Discord (later) | `github.com/bwmarrin/discordgo` | websocket gateway |
| Slack (later) | `github.com/slack-go/slack` (+ Socket Mode) | websocket (Socket Mode) or Events API webhook |
| Matrix / Signal (later) | `mautrix-go` (Matrix; also a Signal bridge) | websocket / sync |

Starting Telegram-only keeps the whole thing pure-Go, so the single-binary story (§5) holds with no sidecars or extra runtimes.

### 7.5 Webhooks vs. long-lived connections

Two delivery styles, and the adapter abstracts which one a channel uses:

- **Long-poll / websocket** (Telegram long-poll, Discord/Slack-Socket-Mode): the adapter holds an outbound connection; **no inbound port, no public URL** needed. Best fit for a self-hosted box behind NAT - and most aligned with NanoClaw's "runs on your machine" ethos. **Telegram long-poll is the default to start.**
- **Webhook** (Telegram webhook mode, Slack Events API): the channel POSTs to you, so you need a public HTTPS endpoint (reverse proxy or tunnel). Lower latency at scale, more infra. Offer it as an option, default to the connection style.

Prefer the connection style by default; it keeps deployment to "run the binary" with no inbound firewall holes.

### 7.6 Security touchpoints

The adapter layer is thin but not security-free:

- **Bot tokens are credentials.** Treat Telegram/Discord/Slack tokens like keys: load from env or the vault (§8), never commit them, never mount them into the agent container - the agent talks to channels only through `outbound.db`, never directly.
- **Untrusted by definition.** Everything an adapter emits is attacker-controllable text/files. It enters the trust-tiering regime (§11.6): inbound content is `untrusted` until reviewed, and attachments routed to the vault land as `draft`.
- **Delivery stays gated.** The adapter will happily `Send` anywhere; the *authorization* (origin-chat-always-allowed + `agent_destinations`) lives in the delivery loop (§9) and must run before `Send` is ever called.

---

## 8. Credential vault (OneCLI Agent Vault)

The vault is a separate concern from the language and ports cleanly. Mechanically, OneCLI works as an **outbound proxy that injects credentials at request time**; the TS side uses `applyContainerConfig()` to point the container's HTTP(S) proxy + CA at the vault so the container never holds raw keys.

In the Go host you replicate this at container-spawn time - it's all env + mounts, language-agnostic:

- Set `HTTP_PROXY` / `HTTPS_PROXY` (and `NO_PROXY`) env vars on the container to the vault endpoint.
- Mount the vault's CA certificate read-only into the container's trust store path.
- Give each non-main agent group its own OneCLI agent identifier for per-group scoping.
- On vault-unreachable: start the container with no credentials and log a warning (match current behavior).

You can keep running OneCLI itself unchanged - it's an external process, not part of the host you're rewriting.

### 8.1 What goclaw actually shipped: a built-in TLS-intercepting proxy

goclaw ships its **own credential proxy** in the host process, no external service:

- **Encrypted store** (`internal/credstore`): credentials live in the central `goclaw.db`, encrypted at rest with AES-256-GCM. The key comes from `GOCLAW_SECRET_ENCRYPTION_KEY` (env only, never the data dir), so a stolen data dir / DB dump doesn't include it. Managed by `goclaw auth add|list|delete` (UUID-keyed, matched by target host).
- **CA + leaf machinery** (`internal/credproxy/ca.go`): a CA (auto-generated under `{data_dir}/proxy/`, or supplied via `GOCLAW_PROXY_CA_KEY`/`_CERT`) that mints short-lived per-host leaf certs on demand, cached and refreshed before expiry. The leaf advertises only `http/1.1` (the intercept loop is HTTP/1.1).
- **TLS-intercepting proxy** (`internal/credproxy/mitmproxy.go`): a host goroutine. For each `CONNECT`, if a credential is stored for the host it terminates the client TLS with a leaf the container trusts, injects the real token per request, and forwards to the real upstream over fresh TLS (SSE-safe). Hosts with no credential are **blind-tunneled** (piped opaquely, never decrypted).
- **Per-host injection scheme**: `api.anthropic.com` -> `x-api-key`; `github.com`/`codeload.github.com` (git smart-HTTP) -> HTTP Basic `x-access-token:<token>` (these reject Bearer); everything else, incl. `api.github.com` -> `Authorization: Bearer`.
- **Wiring**: when any credential is stored, the runner gets `HTTPS_PROXY`/`HTTP_PROXY` (both cases, because git/libcurl read the lowercase form), `NODE_USE_ENV_PROXY=1`, `NO_PROXY` for the proxy host, the CA mounted RO with `NODE_EXTRA_CA_CERTS`/`GIT_SSL_CAINFO`/`SSL_CERT_FILE` pointing at it, and **placeholder** `ANTHROPIC_API_KEY` / `GH_TOKEN` (the latter so `gh` considers itself logged in). No raw token enters the container.

Covers Anthropic + GitHub (`claude`, `git`, `gh`, `curl`), verified live: private clones via both git and gh, chats answer, and the container env holds only placeholders. Fail-open: no stored credential -> fall back to passing the raw tokens. The `internal/vault` stub remains as the alternative path for wiring an external HTTPS-proxy gateway if ever preferred.

---

## 9. Security-model preservation checklist

The port is only correct if every existing boundary survives. Concrete Go/Podman equivalents:

- [ ] **Non-root in container** - `--user 1000:1000`; under rootless Podman this maps to an unprivileged host UID (stronger than before).
- [ ] **PID 1 signal handling** - keep `tini` (or use Podman `--init`, which injects an init process).
- [ ] **Mount allowlist** - read `~/.config/nanoclaw/mount-allowlist.json`; if absent, **block all extra mounts** (fail closed). Never mount the allowlist file itself.
- [ ] **Path validation** - symlink-resolve, reject `..`/absolute/colon; add `:Z` relabeling.
- [ ] **Session DB isolation** - only the two DBs for `{agent_group_id}/{session_id}` are mounted into a given container.
- [ ] **Delivery authorization** - enforce origin-chat-always-allowed + `agent_destinations` checks in the Go delivery loop before dispatch.
- [ ] **Permissions** - port roles, `unknown_sender_policy`, `sender_scope`, and the approval-card flows; these are pure logic + DB rows.
- [ ] **No raw keys in container** - vault proxy as in §8.

---

## 10. Proposed Go project layout

Keeping the spirit of "few files, easy to read":

The module is `github.com/shindakun/goclaw` (the project is **goclaw**; NanoClaw
is the inspiration). The tree below reflects the implemented scaffold:

```
cmd/goclaw/main.go            # entry: load config, DB init, start loops, signal handling
internal/config/              # env + .env loading, defaults (GOCLAW_* vars)
internal/db/                  # central DB open (WAL, single-writer caps), session DB open/close
internal/db/migrations/       # ordered NNNN_*.sql, embedded; applied once each, tracked
internal/router/              # entity resolution + access gate → inbound.db
internal/delivery/            # outbound.db poll, adapter dispatch, delivery authorization
internal/sweep/               # 60s ticker: stale, due-wake, recurrence
internal/runtime/             # Podman lifecycle (CLI shell-out v0), mount builder, runtime select
internal/mounts/              # allowlist load + path validation (security-critical, unit-tested)
internal/permissions/         # roles, sender policy, approval flows
internal/vault/               # OneCLI proxy wiring at spawn time
internal/channels/            # ChannelAdapter interface + fan-in registry
internal/channels/telegram/   # Telegram adapter (the v0 channel, §7.4)
internal/vaultlock/           # OPTIONAL: flock single-writer guard for the shared vault (§11)
container/agent-runner/       # TS Agent SDK (Option A) OR Go on shindakun/agent-sdk-go (Option A′); §4
container/CLAUDE.md           # generic agent-first BASE prompt, baked to /app/CLAUDE.md (§11.0)
container/skills/<name>/      # skills baked into the image (e.g. coding), at /app/skills (§11.0)
internal/vaultinit/           # `goclaw vault init`: embeds template/, installs to ~/Vault (§11)
~/Vault/                      # OPTIONAL: shared knowledge vault - a git repo (§11):
#   ├── CLAUDE.md             #   short vault note pointing at the librarian skill (§11.0)
#   ├── .claude/skills/librarian/SKILL.md  # the vault operating contract; auto-loaded for vault work
#   ├── index.md              #   page catalog (read first)
#   ├── log.md                #   append-only activity log
#   ├── CRITICAL_FACTS.md     #   tiny always-loaded facts file
#   ├── raw/                  #   immutable sources (agent reads, never edits)
#   └── wiki/{entities,concepts,projects,decisions,resources,credentials,daily,tasks}/  # agent-owned notes
```

Two packages beyond the original sketch: `internal/config/` (env + `.env`
loading, kept out of `main` so it's testable) and `internal/channels/telegram/`
(the concrete adapter as a subpackage, so each channel stays isolated from the
interface). Migrations live in `internal/db/migrations/` as ordered, embedded
`.sql` files; a runner applies each exactly once and records it in a
`schema_migrations` table, so startup is idempotent (§5).

The `internal/mounts/` package deserves the most test coverage - it is the one place where a bug becomes a host-filesystem-escape.

---

## 11. Optional layer: a shared knowledge vault

### 11.0 How the prompt is composed (agent-first; the vault is a bolt-on)

goclaw is **agent-first**: the agent has an identity, safety rules, and a coding/ops capability whether or not a vault is mounted. The vault is an *optional* bolt-on brain, never a prerequisite for the agent to function. This is enforced by how the system prompt is built, mirroring NanoClaw's `claude-md-compose.ts` + skill-symlink model:

- A generic **base prompt** (`container/CLAUDE.md`) is baked into the runner image at `/app/CLAUDE.md`: identity, safety, the cross-cutting invariants (do-the-deliverable-not-the-narration, honest reporting, timestamp discipline, no em-dashes), and a mode router. It mentions no vault and no librarian.
- A **coding skill** (`container/skills/coding/SKILL.md`, baked at `/app/skills/coding`) is always available; the model auto-invokes it by `description` on software tasks.
- The **librarian skill** lives in the *vault* (`/vault/.claude/skills/librarian/SKILL.md`), not the image. It carries the entire schema/operating contract below. It is symlinked in (and thus auto-invokable) **only when a vault is mounted**.
- At each launch the host composes the group's `CLAUDE.md` into the container's `~/.claude` as an imports-only entry point (`@./.claude-shared.md` → `/app/CLAUDE.md`), and syncs `~/.claude/skills/<name>` symlinks: `coding` always, `librarian` only with a vault. The targets are container paths (dangling on the host, valid in the container).

So with **no vault** you get a clean coding/ops agent with a real identity; with a vault mounted you get that same agent *plus* the librarian, auto-invoked on knowledge work. The librarian rules (including TWO OUTPUTS and the task-claim/lease discipline) are vault-scoped and never burden a coding turn. The note-writing schema in §11.2-§11.6 below is the *content* of that librarian skill.

### 11.1 The idea

The vault is a directory of Markdown notes that the agent and you both read and write - you browse and edit it in Obsidian, the agent maintains it. The point is to move from query-time retrieval (where the agent re-derives everything from raw sources on every question) to a **compounding knowledge base**: each new source is read once, distilled, and merged into the existing notes, so cross-references, contradictions, and synthesis are already there next time. The vault gets richer with every message and every question instead of starting cold each session.

The mental model inversion is the whole point: **you stop maintaining the notes and the vault rewrites itself with every input.** The human's job shrinks to curating sources and asking good questions; everything else - cross-referencing, contradiction resolution, consistency, synthesis - is the agent's job, and at near-zero marginal cost because the agent does it inline on every ingest and query. Over weeks the vault's density and utility grow non-linearly: each source typically touches ~10-15 existing pages rather than adding one, each good answer becomes a new page, and each maintenance pass surfaces connections nobody named.

**Why this beats RAG at personal scale.** Below roughly 50k-100k tokens (~150-200 dense pages), keeping the knowledge in plain context and letting the agent read the pages it needs gives 100% retrieval reliability, zero vector-DB overhead, and *global* reasoning across the whole corpus instead of stitched-together snippets. NanoClaw should target exactly this regime and lean on `index.md` for navigation (§11.4) - no embeddings, no vector store. Only once a vault outgrows that range does a hybrid (stable core in context, bulk records behind a local BM25/vector search) become worth it. Design for the no-RAG case first.

This fits NanoClaw with almost no new host machinery, because **the vault is just a mount.** The intelligence lives in the agent's instructions (the librarian skill, §11.0) and the agent-runner - not in the Go host. What the host adds is the mount, a write-lock, and a couple of optional hooks.

### 11.2 Where it sits in the architecture

The vault is a single host directory (e.g. `~/Vault/`) mounted **read-write** into the agent-group container at a fixed path (e.g. `/vault`). Obsidian - or any editor, or git - opens the same host directory. Crucially it is **agent-group-scoped and shared across that group's sessions**, so knowledge accumulates instead of being siloed per conversation.

That sharing is a deliberate exception to NanoClaw's per-session isolation, and it's the one place the isolation model bends. Treat it explicitly (see §11.6).

The structure separates content cleanly by **who owns it**, so the agent always knows what it may rewrite:

- `raw/` - **immutable** source material you drop in (articles, PDFs, transcripts, screenshots). The agent reads but never edits these; they are your source of truth.
- `wiki/` - agent-owned Markdown, subdivided by note kind:
  - `entities/` - people, companies, tools (one page each: role, context, history).
  - `concepts/` - ideas, frameworks, and synthesis pages.
  - `projects/` - ongoing work.
  - `decisions/` - short decision records (the *why* behind a choice, ADR-style).
  - `resources/` - links, documents, references worth keeping (one per source).
  - `credentials/` - where a secret lives and how to find it; never the secret itself.
  - `daily/` - day notes.
  - `tasks/` - open/closed task tracking.
- the **schema** - the *behavioral contract*, the programming interface for the agent. Folder map, naming conventions, frontmatter schemas, create-vs-edit rules, link rules, reconcile procedure, lint criteria, and the note-writing discipline below. In goclaw this lives in a **librarian skill** (`/vault/.claude/skills/librarian/SKILL.md`), NOT in the agent's identity prompt - see §11.0. The agent auto-loads it when it does vault work, which keeps it a disciplined librarian rather than drifting into generic-chatbot mode, *without* forcing every coding/ops turn through the librarian rulebook.

A few small **always-loaded context files** at the vault root make the agent cheap to orient on every turn:

- `index.md` - the page catalog (read first; see §11.4).
- `log.md` - append-only activity log (see §11.4).
- `CRITICAL_FACTS.md` - a tiny (~100-150 token) file of facts that must never be wrong or forgotten, loaded on every run regardless of budget. The host enforces this: when a vault is mounted, the composed entry-point `CLAUDE.md` imports `/vault/CRITICAL_FACTS.md` directly (see §8 prompt composition), so the L0 facts are present every turn rather than depending on the agent choosing to open the librarian skill.
- optionally an identity/`SOUL.md`-style file describing the owner and the vault's purpose, so synthesis stays grounded in who it's for.

### 11.3 Notes written for the agent, not for reading

The highest-leverage design choice: write notes for *future-agent retrieval*, not human browsing. A note pulled by search later may arrive with no surrounding context, so each note must carry its own. Concretely, every agent-written note should have:

- a 2-3 sentence summary preamble at the top (a "for future retrieval" header), so the agent can judge relevance at a glance before reading the whole note;
- machine-readable frontmatter (`type`, `date`, `state`, and type-specific fields) - this also lets Obsidian's Dataview query the vault;
- **two tag axes**: a coarse `domain` (the life area - home / health / work) and fine `tags` (topics within it); filtering by domain narrows fast, tags pinpoint;
- a structured `entities` list (`[{name, kind, role, org}]`) alongside the prose, so the agent can do "find every note mentioning a plumber" lookups that `[[wikilinks]]` alone don't support;
- recency markers on volatile claims - e.g. "raised $24M (as of 2026-04, <source-url>)" - so the agent knows what may be stale;
- source provenance preserved inline for every external claim (URL plus, where it matters, the quoted line); an `unresolved_reference` field records a cross-link the writer couldn't resolve at write time, for the reconcile pass to close;
- mandatory `[[wikilinks]]` to every person / project / idea / decision mentioned, so the graph stays traversable;
- a confidence tag where it matters (`stated | high | medium | speculation`).

Humans still read it fine in Obsidian; the structure simply optimizes for the agent being the primary reader.

**Bi-temporal facts.** Don't silently overwrite a claim when a newer source changes it - record both *when it was true* and *when the vault learned it changed*. ("Believed X; after ingesting Y on 2026-04-12, shifted to Z.") This keeps an audit trail of the agent's own reasoning and makes contradiction resolution reviewable instead of a black box.

**Page lifecycle.** Give every page a `state` in its frontmatter and let it move through a fixed set: `draft → active → stale → contradicted → archived`. The agent advances state during reconcile/lint rather than deleting pages, so superseded knowledge stays auditable and revertible. (A `draft → verified` gate, §11.6, is the trust variant of the same idea.)

**The two-output rule.** Every answer produces *two* outputs: the reply to you, and a vault update. A decision yields a decision note; a research answer rewrites the handful of pages it touched and files the synthesis as a new page. This is what makes the vault *compound* rather than *bloat* - explorations accrete the same way ingested sources do, instead of evaporating into chat history.

### 11.4 Operations

The core operations are the librarian's day job:

| Operation | What the agent does |
|---|---|
| **Ingest** | Read a new source; search the vault for the entities/concepts it touches; **update the existing pages** rather than just appending a new one, creating pages only when nothing matches; add wikilinks; append a `log.md` entry. One source typically touches 10-15 pages. |
| **Query** | Search the vault first, read the relevant notes, synthesize an answer with citations - and file good answers back as new notes (the two-output rule), so explorations compound too. |
| **Reconcile** | Find notes that contradict each other and *resolve* them by comparing sources, dates, and confidence - not merely flag the conflict - recording the rationale and advancing page `state`. |
| **Synthesize** | Scan for recurring but un-named themes and write synthesis pages on its own initiative. |
| **Health / lint** | Audit for broken links, duplicates, missing or malformed frontmatter, stale claims, and orphan pages (no inbound links). Report by severity; never auto-fix. |

A second tier of **thinking tools** turns the vault from a passive store into something that pushes back - these are where the vault earns its keep:

| Tool | What it does |
|---|---|
| **Challenge** | Argues *against* a proposed idea using your own history - past decisions, prior failures, contradicting notes - before you commit. |
| **Emerge** | Surfaces patterns across notes you never explicitly named. |
| **Connect [A] [B]** | Bridges two unrelated pages/domains to spark a non-obvious link. |

Two navigation aids make this work without embedding-based RAG at personal scale: an `index.md` catalog (every page, a one-line summary, updated on each ingest) and an append-only `log.md` with a consistent line prefix so it stays greppable (`grep '^## \[' log.md | tail`). Add a local Markdown search tool (shell-out or MCP) only once the vault outgrows the index.

**Progressive context loading.** Don't load the whole vault. Define budget tiers the agent climbs only as needed: **L0** = `CRITICAL_FACTS.md` + identity (always, ~hundreds of tokens); **L1** = `index.md` to locate relevant pages; **L2** = the specific pages a task touches; **L3** = follow `[[wikilinks]]` outward only when the answer demands it. Most turns never go past L1-L2, which keeps the no-RAG model affordable even as the vault grows.

Invariants worth enforcing in the schema: **search before create** (no duplicates; fuzzy-match existing names), **propagate every write** (a new note updates the index and any linked pages), **no orphans** (every note is linked from somewhere), and **provenance-first** (no claim without a source). During lint, **randomize the page-visit order** rather than walking ingestion order - it surfaces cross-cutting contradictions that an ordered pass tends to miss.

Two more, because the vault is shared by multiple agent runs and the human at once (the same breach of isolation §11.6 handles at the host level). **Task leases**: a `task` note carries `claimed_by` + `lease_until`; a worker claims before working, heartbeats to extend, and a claim whose lease has passed is *stale and reclaimable* - so two runs never grind the same task, and a crashed run's work isn't locked forever. Don't-finish becomes `state: blocked` with a handoff note, not abandonment; tasks are never deleted. **Append-only audit trail**: every mutating action appends one `log.md` line naming the actor (agent run or human) and the pages touched, and `log.md` is never rewritten - this is what makes the bi-temporal and lifecycle rules actually auditable (you can reconstruct how any page reached its state) rather than merely asserted. Both are pure schema + prompt discipline; the only host-side support they need is the `flock` write guard already in §11.5.

### 11.5 What this costs the Go host (not much)

The vault is mostly configuration plus prompt discipline. The host-side pieces are small:

1. **The mount.** One read-write allowlist entry for the vault path, mounted `:Z` under Podman. Everything in §6.3 already covers this - the vault is just the deliberate RW exception to the otherwise read-only default.
2. **A write lock.** The vault now has several possible writers - multiple sessions of the group, the scheduled maintenance runs, and you in Obsidian. To keep NanoClaw's one-writer-at-a-time discipline, have the Go host take a `flock` on a vault lockfile before launching any vault-mutating agent run and release it after; reads never block. This closes the concurrent-write corruption hole directly (`internal/vaultlock/`).
3. **A write-time validator (optional, worth it).** A small check that fires on every note write and warns when the AI-first rules are violated (missing preamble, missing frontmatter field, broken YAML). Two placements: as an agent-side `PostToolUse` hook (best - the agent sees the warning and repairs the note in the same turn), or as a Go host watcher on the vault directory via `fsnotify` (a backstop that works no matter which provider the group runs). The validator warns; it never reverts.
4. **Scheduled maintenance - an always-on cadence.** NanoClaw already has scheduled tasks, so point a recurring rhythm at the vault and it maintains itself while you sleep:
   - **morning** - build the day note: pull due/overdue tasks and (if wired) calendar events into `daily/`.
   - **nightly** - consolidation pass: reconcile contradictions, synthesize new themes, heal orphan pages.
   - **weekly** - review: summarize the week's progress and surfaced patterns.
   - **weekly health audit** - the full lint sweep (broken links, duplicates, stale claims, malformed frontmatter), reported by severity.

   No new infrastructure; these are just scheduled agent runs against `/vault`. A light **save nudge** is worth adding too: after a long exchange (or when you say "done"), the agent offers to extract decisions/tasks/people into the vault before context is lost.

Two integrations fall out for free:

- **Channels become ingest pipes.** Forward an article, PDF, or screenshot to your agent on Telegram and the ingest flow files it into the vault.
- **Vault-first research.** Before hitting the web, the agent checks what the vault already knows and researches only the gaps, returning a delta (new / confirmed / now-contradicted). You stop re-researching what you've already filed.

Keep the command/skill set **provider-neutral** so the vault behaves identically whether an agent group runs Claude or an `/add-opencode` / `/add-ollama` provider - the notes are just Markdown either way. Optionally seed a group with a **role preset** that sets sensible folders and boards out of the box - e.g. *builder* (backlog / sprint / done), *researcher* (reading / processing / synthesized), *executive* (OKRs / quarterly / weekly), *creator* (ideas / drafts / published).

### 11.6 Security notes specific to the shared vault

The shared, writable vault is the one spot that crosses session isolation, so handle it explicitly:

- **Prompt-injection can poison the wiki.** A malicious message in one session could get the agent to write false claims that another session later trusts. Mitigations: keep the vault **Markdown-only** (reading a note never executes anything), **scope each vault to a single agent group** (never share one vault across groups), and lean on the AI-first rules - verbatim sources plus confidence tags make injected claims auditable rather than laundered into fact.
- **Trust-tier untrusted input.** Treat anything arriving from a channel or an external source as **untrusted until reviewed**. Tag it as such in frontmatter and don't let it land as `verified`/`active` knowledge on first write - gate it behind the `draft` state, and for high-stakes writes have a second, independent agent pass review the proposed edit before it's committed to the wiki. The page lifecycle (§11.3) is the mechanism: untrusted claims enter as `draft` and only a deliberate step promotes them.
- **git is the safety net.** The vault is a git repo, so every agent edit is a diff you can review and revert, and a team can share it by cloning. Consider a `draft → verified` frontmatter state if you want a human gate before notes are treated as trusted.
- **The vault still lives under the mount-allowlist regime.** It is read-write only because you explicitly allowed that one path; the blocked-pattern list (`.ssh`, `.env`, credentials, …) and the path validation in §6.3 / §9 are unchanged.

### 11.7 Quick-start: a minimal vault schema

Everything above is configuration plus prompt discipline - the host barely changes. In goclaw the discipline lives in the **librarian skill** at `/vault/.claude/skills/librarian/SKILL.md` (§11.0), auto-loaded for vault work; the vault-root `CLAUDE.md` shrinks to a short note pointing at it. Here is a minimal-but-complete skeleton (the body of that SKILL.md) an agent group can run with on day one; trim or extend per role preset.

````markdown
---
name: librarian
description: Knowledge-vault librarian discipline. Use for vault work; not for plain coding.
---
# Vault Librarian

When working in the vault you are its librarian, not a chatbot. Every vault turn
reads from or writes to it. Obey this contract; do not improvise structure. (The
cross-cutting rules - do the deliverable, report honestly, timestamps - come from
the base prompt; this skill adds the vault-specific discipline.)

## Layout
- `raw/`             immutable sources - READ ONLY, never edit.
- `wiki/entities/`   people, companies, tools - one page each.
- `wiki/concepts/`   ideas, frameworks, synthesis.
- `wiki/projects/`   ongoing work.
- `wiki/decisions/`  decision records (the *why* of a choice).
- `wiki/resources/`  links, documents, references (one per source).
- `wiki/credentials/` where a secret lives - never the secret itself.
- `wiki/daily/`      day notes (`YYYY-MM-DD.md`).
- `wiki/tasks/`      open/closed tasks.
- `index.md`         page catalog - READ FIRST, update on every write.
- `log.md`           append-only activity log.
- `CRITICAL_FACTS.md`  tiny always-true facts - load every turn.

## Context budget (climb only as needed)
- L0: CRITICAL_FACTS.md + identity        (always)
- L1: index.md                            (to locate pages)
- L2: the specific pages the task touches
- L3: follow [[wikilinks]] outward        (only when the answer needs it)

## Frontmatter (required on every wiki note)
---
type: entity | concept | project | decision | resource | credential | daily | task
state: draft | active | stale | contradicted | archived
date: YYYY-MM-DD
domain: []                        # COARSE life area(s): home | health | work | …
tags: []                          # FINE topic tags (≤3) within the domain(s)
trust: trusted | untrusted        # channel/external input starts untrusted
confidence: stated | high | medium | speculation
entities: []                      # [{name, kind, role, org}] - structured lookup
unresolved_reference:             # a reference noted but not yet resolved
---

## Note shape
1. A 2-3 sentence summary preamble at the very top (judge relevance before reading on).
2. Body, with a source URL inline beside every external claim.
3. Recency markers on volatile facts: "raised $24M (as of 2026-04, <url>)".
4. `[[wikilinks]]` to EVERY person / project / idea / decision named.

## Invariants (never violate)
- SEARCH BEFORE CREATE - fuzzy-match existing names; update the page, don't duplicate.
- PROPAGATE EVERY WRITE - update index.md and every linked page; append to log.md.
- NO ORPHANS - every note is linked from somewhere.
- PROVENANCE-FIRST - no claim without a source.
- TWO OUTPUTS - every answer also files a vault update (decision → decision note,
  research → rewrite touched pages + a synthesis page).
- BI-TEMPORAL - don't overwrite a changed fact; record what was believed, what
  changed it, and when. Move the old claim's `state`, don't delete it.
- UNTRUSTED IN → DRAFT - external input lands as `state: draft, trust: untrusted`;
  promotion to `active` is a deliberate, reviewed step.

## Operations
- ingest <source>   read it → search vault → update the 10-15 pages it touches →
                    create only what's missing → link → log.
- query <question>  search vault first → read relevant pages → answer with citations
                    → file the answer back as a note.
- reconcile         find contradicting notes → RESOLVE by source/date/confidence
                    (don't just flag) → record rationale → advance state.
- synthesize        find recurring un-named themes → write synthesis pages.
- lint              broken links, dupes, bad frontmatter, stale claims, orphans →
                    report by severity, NEVER auto-fix. Visit pages in random order.

## Thinking tools
- challenge <idea>  argue AGAINST it using this vault's history and past failures.
- emerge            surface patterns I never explicitly named.
- connect <A> <B>   bridge two unrelated pages for a non-obvious link.

## log.md line format (keep greppable)
## [YYYY-MM-DD HH:MM] <ingest|query|reconcile|lint> - <one line> - pages: a.md, b.md
````

`log.md` stays a flat, greppable timeline (`grep '^## \[' log.md | tail`), and `index.md` is just a categorized list of `[[page]] - one-line summary`. The scheduled maintenance jobs (§11.5) call `reconcile` / `synthesize` / `lint`; the channel ingest path (§11.5) calls `ingest`. Nothing here is Claude-specific - the same file drives an OpenCode or Ollama group unchanged.

---

## 12. Risks and open questions

1. **Channel scope is deliberately narrow to start.** v0 ships **Telegram only** - pure-Go, low-risk, no sidecars (§7.4). Discord and Slack drop in later behind the same `ChannelAdapter` interface with no host changes. (WhatsApp is intentionally out of scope: the reference Baileys stack drags in a Node keystore and LID-mapping complexity with no equally-mature Go library, and it isn't needed to prove the design.)
2. **"Customization = code changes" + Claude Code.** A selling point is that Claude Code reads and edits the NanoClaw codebase. Claude Code is language-agnostic, so this still works on a Go codebase - but the existing `/add-<channel>` and `/customize` skills assume TS module paths and will need rewriting for the Go layout.
3. **Installer.** `nanoclaw.sh` bootstraps Node + pnpm + Docker. The Go version drops Node/pnpm (single binary) and installs Podman instead - net simpler, but the script and the Claude-Code error-recovery handoffs need reworking.
4. **macOS rootless Podman.** On macOS, Podman runs containers inside a managed Linux VM (`podman machine`). Behavior is fine but mount paths and performance differ from Docker Desktop; test mount semantics there explicitly.
5. **Runner language & the Go Agent SDK dependency.** Option A (TS runner) keeps two languages in the repo - fine given the clean SQLite boundary, but a deviation from single-language simplicity. Option A′ (Go runner on `shindakun/agent-sdk-go`) removes that deviation but adds a dependency on a third-party port: vet its parity claims against your Claude Code CLI version, confirm it survives your real message traffic, and note it still requires the `claude` CLI (hence Node) inside the container image. The staged plan (ship host on TS runner, then migrate) de-risks this - you're never blocked on the Go runner to make progress. Only Option B (Client SDK, hand-built loop) drops the Node/CLI dependency entirely, at the cost of the whole harness.
6. **Shared vault vs. session isolation.** The optional knowledge vault (§11) is a writable mount shared across an agent group's sessions, so it deliberately breaches per-session isolation. It needs the `flock` write guard, single-group scoping, and git-revert safety net from §11.6 - and a vault must never be shared across agent groups.

---

## 13. Suggested phased plan

1. **Phase 0 - Spike the boundary.** Stand up a Go process that opens an `inbound.db`/`outbound.db` pair, spawns a container via `podman run`, and round-trips one message to the *existing* TS agent-runner. Proves the SQLite boundary (Option A) end-to-end before any runner rewrite.
2. **Phase 1 - Host core.** Port DB + migrations (identical schema), router, session manager, delivery, sweep. Single channel (Telegram) for testing.
3. **Phase 2 - Runtime hardening.** Mount validation + allowlist, `:Z` relabeling, rootless user mapping, `--init`/`tini`, runtime selection (crun default; gVisor/Kata opt-in).
4. **Phase 3 - Permissions + credentials.** Roles, sender policies, approval flows, OneCLI credential-vault proxy wiring, delivery authorization.
5. **Phase 4 - More channels + ops.** Additional adapters (Discord, then Slack - same `ChannelAdapter` interface, §7), Quadlet/systemd units, installer rewrite, rework `/add-*` and `/customize` skills for the Go layout.
6. **Phase 5 - Knowledge vault (optional).** Read-write vault mount + `flock` write guard; the AI-first schema in the group `CLAUDE.md` (folder map, frontmatter, bi-temporal facts, page lifecycle, two-output + propagation invariants); the always-loaded context files (`index.md` / `log.md` / `CRITICAL_FACTS.md`) and progressive L0-L3 loading; the write-time validator hook; the ingest/query/reconcile/synthesize/lint operations plus the challenge/emerge/connect thinking tools; scheduled maintenance (morning / nightly / weekly / health); trust-tiering for untrusted channel input; and the ingest-from-channel path. Target the no-RAG regime (≤~100k tokens); defer hybrid search until the vault outgrows it.
7. **Phase 6 - (Optional) Go runner, single language (Option A′).** Once the host is stable on the TS runner, port `container/agent-runner/` to Go on [`shindakun/agent-sdk-go`](https://github.com/shindakun/agent-sdk-go). The runner still drives the `claude` CLI, so the loop, tools, Skills, and subagents are preserved; the win is one language across the repo. Validate the SDK's parity against your CLI version and run it against real traffic in parallel with the TS runner before cutting over. (Only fall back to a Client-SDK hand-built loop - old Option B - if dropping the `claude`-CLI/Node dependency is itself a hard requirement.)

---

## 14. Sources

- NanoClaw site & README - https://nanoclaw.dev/ , https://github.com/nanocoai/nanoclaw
- NanoClaw security model - https://docs.nanoclaw.dev/concepts/security
- Anthropic client SDKs (Go supported) - https://platform.claude.com/docs/en/api/client-sdks , https://github.com/anthropics/anthropic-sdk-go
- Official Agent SDK ships for TS/Python (drives the `claude` CLI subprocess) - https://github.com/anthropics/claude-agent-sdk-python/issues/498
- **Go Agent SDK** (port; drives the `claude` CLI, enables Option A′) - https://github.com/shindakun/agent-sdk-go
- Agent SDK vs Client SDK (who implements the tool loop) - https://code.claude.com/docs/en/agent-sdk/overview
- Managed Agents (hosted; supports Go) - https://platform.claude.com/docs/en/agents-and-tools/agent-skills/claude-api-skill
