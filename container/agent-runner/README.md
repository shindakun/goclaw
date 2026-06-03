# container/agent-runner/

The in-container agent runner - the process that runs **inside** each
per-agent-group container, polls `inbound.db`, runs Claude, and writes replies
to `outbound.db` (brief §3.1, §4).

This is deliberately **not** part of the Go host module. The host (`cmd/goclaw`
+ `internal/...`) only ever touches the two SQLite files and the container
lifecycle; it never imports this code. The boundary is the SQLite pair, so the
runner can be written in a different language than the host.

## Two options (brief §4)

- **Option A - TypeScript** (lowest-churn to start): keep the existing NanoClaw
  agent-runner on the official Claude Agent SDK (TS), unchanged. The Go host
  spawns its container and round-trips messages through the DB pair.
- **Option A′ - Go** (single-language end state): rewrite the runner in Go on
  [`shindakun/agent-sdk-go`](https://github.com/shindakun/agent-sdk-go). It still
  drives the `claude` CLI subprocess, so the agent loop, built-in tools, Skills,
  and subagents are preserved.

Either way the container image needs the `claude` CLI on PATH (hence Node),
installable via npm.

## Status

Placeholder. For now the boundary is proven on the host with
[`cmd/stub-runner`](../../cmd/stub-runner/) - a minimal echo runner that reads
`inbound.db` and writes `outbound.db`, no Claude. The next step is to run that
(then the real runner) inside a Podman container via `internal/runtime`. Drop
the production runner implementation (TS sources, or a Go `main` +
Containerfile) here.
