# Cleanup punch list

Compiled 2026-06-12 from a repo sweep (deadcode analysis, TODO scan, doc-vs-code
drift check). Each item is small and self-contained unless marked otherwise.
Re-check dead-code claims with: `go run golang.org/x/tools/cmd/deadcode@latest ./...`
(it does whole-program analysis from the main packages, so it correctly handles
interface dispatch; a plain caller grep does not).

Ground rules for whoever picks these up: tests with the change, `go test ./...`
plus `golangci-lint run ./...` before committing, no em-dashes anywhere, and the
pre-commit hook gates the rest. Both are currently green; keep them that way.

## 1. Wire `internal/vaultlock` or descope it deliberately (the only non-trivial item)

- [ ] **Evidence:** deadcode reports every function in `internal/vaultlock`
  (`Acquire`, `TryAcquire`, `Lock.Release`) unreachable, and no production code
  imports the package. It was scaffolded in the very first commit and never wired.
- **Why it matters:** this is not just dead code. The brief
  (`docs/nanoclaw-go-podman-brief.md`, the "write lock" item and §11.5/§602)
  specifies the host takes a flock on a vault lockfile before launching any
  vault-mutating agent run, because the vault has several writers: a group's
  sessions, scheduled maintenance runs, `goclaw vault sync`, and the human in
  Obsidian. That guard is documented as the thing closing the concurrent-write
  hole, and it is not enforced anywhere today.
- **Action:** decide first, then implement: either (a) wire it: take the lock at
  the vault-mutating choke points (`goclaw vault sync` in `cmd/goclaw/vault.go`,
  and around vault-writing runner activity per the brief's design; that second
  part needs a short design pass on granularity, since a long-lived warm container
  is not a discrete "run"), or (b) descope: delete the package and amend the brief
  and `CLAUDE.md` so the docs stop promising an unenforced guard. Do not leave the
  middle state.
- **Size:** medium. The decision is the work; the code either way is small.

## 2. Stale package comment: `internal/channels/plugin/adapter.go` header

- [ ] The last paragraph of the package doc says the only `ChannelClient`
  constructor is `plugin.LaunchChannel` (host-spawned, chantest/tests only) and
  that "this package is wired ONLY by chantest/tests, never by cmd/goclaw."
  Both claims are stale: `plugin.AttachChannel`
  (`internal/plugin/channel_client.go:92`) attaches over an already-connected
  socket, and `cmd/goclaw` wires this package in production via
  `setupChannelPlugins` -> `relay.Open` -> `NewAdapter` (and now also live via
  the `channelActivator` on `/plugin add`). Rewrite the paragraph to describe
  the shipped state; keep the security-model paragraph (it is still correct).

## 3. Doc drift: `docs/plugin-updates.md` phase 1 parenthetical

- [ ] Phase 1 says "(Remaining nice-to-have from this phase: surface provenance
  in `goclaw plugin list`, which is not yet wired.)" It is wired: `/plugin list`
  prints `@<shortcommit>` from the `.source.json` sidecar
  (`internal/router/router.go`, `pluginList`). Update the doc. While in there,
  confirm the rest of the phase list still matches reality.

## 4. `db.ClaudeHomeDir` is dead and the layout is duplicated

- [ ] `internal/db/db.go:142` (`ClaudeHomeDir`) is unreachable. The real path is
  built inline by `claudeHomeFor` at `internal/runtime/runtime.go:179`
  (`filepath.Join(dataDir, "claude-home", id)`). Two places encode the
  `claude-home/<id>` layout and only one is used; that is drift risk.
  **Action:** make one owner: either have `runtime` call `db.ClaudeHomeDir`
  (check the import is acceptable; `runtime` is the higher layer so it should
  be) or delete the `db` helper and leave a comment in `runtime` that it owns
  the layout.

## 5. Dead exported path helpers in `internal/runtime/session.go` + three-way constant duplication

- [ ] `PluginsContainerPath()` (session.go:51) and `ChannelSockContainerPath()`
  (session.go:60) are unreachable. Their doc comments claim they exist "so the
  runner knows where" / "so the host and runner agree", but nothing calls them;
  instead the same constants are duplicated by hand in
  `internal/channels/plugin/relay.go:21` (`channelSockContainerPath`, with a
  "kept local to avoid importing runtime (cycle risk)" note) and in
  `cmd/claude-runner` (`channelSocketDir`), each with a "must match" comment.
  **Action:** at minimum delete the two dead exported funcs. Better: pick one
  home for the in-container path constants that all three sides can import
  without a cycle (a tiny leaf package, or `internal/plugin`, which both ends
  already import) and collapse the "must match" comments into real imports.

## 6. Stale manifest comment

- [ ] `internal/plugin/manifest.go:20`: `Kind string ... // "tool" now
  ("channel" later)`. Channel plugins shipped; the validation below it already
  accepts both. Fix the comment to `// "tool" or "channel"`.

## 7. Rewrite the two stale TODOs in `internal/runtime/runtime.go`

- [ ] `runtime.go:203` (above `Run`): "TODO: capture stdout/stderr, detached vs.
  foreground, restart policy, and teardown." Half is done: the function right
  below captures stderr and surfaces it in errors, and teardown exists (`Stop`,
  plus the sweep's idle GC). Rewrite the TODO to only what is genuinely missing
  (restart policy, stdout capture if still wanted) or delete it.
- [ ] `runtime.go:254` (`Stop`): "TODO: implement `podman stop` with a grace
  period." `podman stop` already applies a 10s default grace. Either pass an
  explicit `-t <seconds>` (and decide the number) or delete the TODO.
  Note: `Stop` IS reachable (via the runners interface; deadcode confirms), so
  do not delete the method itself. An earlier codegraph "no callers" result was
  a dynamic-dispatch miss.

## 8. `.env.example` is missing documented vars

- [ ] `GOCLAW_CHANNEL_TRANSPORT` (read at `internal/config/config.go:131`,
  default "tcp"; "unix" is the native-Linux option) is absent from
  `.env.example`.
- [ ] `GOCLAW_TRANSCRIPT_ROTATE_BYTES` and `GOCLAW_TRANSCRIPT_ROTATE_AGE_DAYS`
  (read in `cmd/claude-runner/rotate.go:51,56`, forwarded by the host at
  `cmd/goclaw/main.go:305`) are also absent. Add commented entries with the
  defaults so operators can discover them.

## 9. Trivial dead code (do in one commit)

- [ ] `internal/plugin/client.go:86` `Client.Name()`: unreachable. Delete, or
  keep only if a near-term caller is known.
- [ ] `internal/plugin/semver.go:60` `semver.String()`: unreachable, but
  `String()` methods are conventional Go; keeping it is defensible. Either
  delete or leave deliberately; just decide.

## Explicitly NOT on this list

- Outbound attachments end to end: a boundary change (outbound.db schema +
  runner convention), tracked in the README "Next" list, not a cleanup.
- Live verification of channel-plugin hot-reload with a real `/plugin add`
  against a running host: worth doing, but it is a verification task, not a
  cleanup.
- `cmd/chantest` having no tests: it is a dev harness, fine as is.
