# Security model

This documents goclaw's threat model and the controls that enforce it. It is the
reference for "what is goclaw trying to protect against, and how."

## Threat model

Three things are treated as untrusted:

1. **Inbound chat input.** Anyone can message a channel the bot is on. Sender
   identity, message content, and chat ids are attacker-controlled.
2. **The agent itself.** The Claude agent runs arbitrary tools (bash, git, clones
   of third-party repos) and can be steered by prompt injection from a message or
   a cloned repo. So the agent is assumed potentially hostile, and the container
   is the security boundary.
3. **Plugins.** A plugin is third-party code downloaded from a git URL and
   compiled. It is assumed potentially hostile, so it runs inside the same
   container sandbox as the agent, never on the host (see "Plugins run in the
   sandbox").

The host process (the Go orchestrator) is trusted. If the host or its filesystem
is fully compromised, all bets are off; that is outside this model.

## Container is the sandbox

Each agent group runs in its own Podman container with security defaults applied
at launch (`internal/runtime`):

- `--user 1000:1000` - non-root inside the container.
- `--init` - a real PID 1 for signal handling.
- Rootless Podman, crun by default.
- No `--privileged`, no host networking, no Docker/Podman socket mounted.
- The agent reaches only its mounts (`/sessions`, `/home/agent/.claude`, `/work`,
  optionally `/vault`, the proxy CA) and the network. It cannot touch the host
  filesystem.

The container launch argv is assembled with `exec.Command` (never a shell), and
every value that ends up on the podman command line is either host-controlled or
validated, so no chat input or agent output can inject podman flags or shell
metacharacters.

## Plugins run in the sandbox, never on the host

Plugins are an extension mechanism: small compiled binaries that add tools (and
channels; the outbound-dialer channel path is in progress, see
`docs/channels-plugin-design.md`). Because a plugin is third-party code the operator
downloaded and compiled, it is untrusted, and where it RUNS is the security decision.
The same sandbox rule governs channels: untrusted channel code runs in the container,
never on the host, which is why the host CONNECTS to a sandboxed channel plugin rather
than executing the binary itself.

goclaw never executes a plugin on the host. The host stages plugin binaries into a
`plugins/` directory and mounts that directory READ-ONLY into the agent container
(`/plugins`). The in-container runner (`cmd/claude-runner`) discovers and launches
each plugin there, so untrusted plugin code inherits exactly the agent's sandbox:

- non-root (`1000:1000`), rootless, no host filesystem beyond the container's
  mounts, no host network namespace, and it dies with the container;
- read-only `/plugins`, so a plugin cannot rewrite its own install or another
  plugin's binary;
- a minimal, allowlisted environment. A plugin process is NOT handed the runner's
  environment. It gets only the env var NAMES its own `plugin.yml` `env:` list
  declares, on top of a secret-free PATH-only base (`Manifest.InjectEnv` /
  `MinimalEnvBase`). This matters because in the direct-env credential mode the
  container's own environment holds a real `ANTHROPIC_API_KEY` / `GH_TOKEN`; the
  allowlist is what stops a hostile plugin from reading them out of the environment.
  (With the credential proxy active those container vars are only placeholders, so
  this is defense in depth on top of the proxy.)
- no access to host credentials. The host's secrets never enter the container
  (the credential proxy injects tokens on the wire; see below), so a malicious
  plugin cannot read them.

The blast radius of a hostile plugin is therefore the container's own mounts
(`/sessions`, `/vault`), the same surface the agent already has, not the host. The
host process and its filesystem, which the threat model trusts, are never exposed
to plugin code.

This is a deliberate departure from how the related "claw" systems extend
themselves, and it is the main security advantage of goclaw's plugin model:

- OpenClaw runs everything in ONE process: no container, no isolation. An extension
  runs with the full process's privileges.
- NanoClaw splits host and an in-container agent runner, and its tools do run in
  that runner. But extensions are TypeScript merged into the source tree and the
  app is recompiled, so the "plugin" is source you build into the host/runner, with
  whatever trust that implies, and updates are git merges into your fork.
- goclaw plugins are isolated, separately-compiled BINARIES the runner launches as
  child processes inside the sandbox. They are never merged into goclaw's source,
  never run on the host, and adding or removing one neither rebuilds nor restarts
  the host. Untrusted code stays code-you-run-in-a-box, not code-you-compile-into-
  yourself.

### Installing a plugin: untrusted code never builds on the host either

`/plugin add <git-url>` (owner-only) installs a plugin, and the install itself is
sandboxed, not just the runtime. The host does NOT clone, scan, or `go build` the
untrusted source. Instead the whole pipeline runs inside a throwaway, rootless,
non-root container (the runner image, which carries git and the Go toolchain):

1. **Bare public clone.** A shallow `git clone` of a PUBLIC repo, with
   `GIT_TERMINAL_PROMPT=0` so a private URL fails fast rather than prompting. No
   credentials are involved and the credential proxy is deliberately NOT in this
   path: plugin installation and the agent's runtime API auth are separate concerns,
   and a public clone needs no secret. Private-repo support is intentionally out of
   scope for now (a documented follow-up would inject a stored token into the
   in-container clone).
2. **Red-flag scan, before building.** The source is rejected if it uses cgo
   (`import "C"`, which compiles/links arbitrary C), `//go:generate` (runs arbitrary
   commands at build time), or a `go.mod` `replace` directive (pulls code from
   anywhere); and it must import `goclawkit` (a real goclaw plugin does). These are
   the build-time code-execution vectors, refused before `go build` runs.
3. **Pure-Go, pinned build.** `CGO_ENABLED=0 GOOS=linux go build` produces a static
   Linux binary; CGO-off both enforces purity and matches how the plugin must run.
   The exact source commit is recorded so an install is reproducible and an update
   is an explicit re-fetch.
4. **Only the artifact leaves the sandbox.** The clone and `go build` happen in the
   container's own `/work`; the host never sees the source. Only the built binary,
   its `plugin.yml`, and the pinned commit are handed back, through the single
   mounted `/out` dir, which on the host is a staging area (`data/plugins-staging/`)
   separate from the watched plugins dir. The host then copies just those files into
   `data/plugins/<name>/` (atomically, via a hidden rename), where the runner's
   filesystem watch loads the new plugin live (no host or container restart). None
   of the untrusted source ever reaches the host filesystem.

So untrusted plugin code is isolated at BOTH points it could run: at build (in the
sandbox container) and at runtime (in the agent's sandbox). The host orchestrator,
which the threat model trusts, never executes plugin author code at any stage.

(Note: this is "rootless container with explicit mounts", a strong, real boundary,
not a microVM. A container escape is out of scope here, as is host compromise.)

## Credential proxy: no raw tokens in the container

The agent needs to call Anthropic and GitHub, but must not hold the raw tokens
(a prompt-injected agent running `echo $ANTHROPIC_API_KEY` must get nothing).
The credential proxy (`internal/credproxy`, `internal/credstore`, brief §8)
achieves this:

- Tokens are stored encrypted at rest (AES-256-GCM, random nonce per record) in
  the central DB. The encryption key comes from `GOCLAW_SECRET_ENCRYPTION_KEY`
  (env only, never the data dir), so a stolen data dir or DB dump does not include
  it.
- The runner routes its HTTPS through a host-side TLS-intercepting proxy and
  trusts the proxy's CA. The proxy injects the real token per request at the host
  boundary and forwards over real (verified) TLS to the upstream.
- The container only ever holds the literal string `placeholder` for
  `ANTHROPIC_API_KEY` / `GH_TOKEN`. The real tokens stay in host memory.
- This is the **recommended** way to provide credentials. The direct-env path
  (`GOCLAW_ANTHROPIC_API_KEY`, `GOCLAW_GITHUB_TOKEN`) is the simpler fallback and
  puts the raw token inside the container.

Property: the agent can **use** a credential (make authenticated calls) but cannot
**read** it. An injecting proxy never exposes the token to the caller; an API does
not echo your auth header back. The agent gets the capability, not the secret.

### Proxy CA

The proxy mints short-lived per-host leaf certs from a CA the container trusts:

- The CA private key uses `crypto/rand`, is persisted at mode `0600` under
  `{data_dir}/proxy/`, or supplied via `GOCLAW_PROXY_CA_KEY` / `_CERT`.
- Each leaf's SAN is strictly the requested host (no wildcards), so the proxy can
  only present a cert for the exact host it is intercepting.
- Leaves are short-lived (24h) and cached.
- The CA is trusted only inside the container (the sandbox). It is host-local; the
  same compromise that exposes it already owns the host.
- Hosts with no stored credential are **blind-tunneled**: the proxy pipes bytes
  without decrypting, so that traffic stays end-to-end encrypted to its real
  destination.

## Access gate: who can reach the agent

Inbound messages pass an access gate (`internal/permissions`, `internal/router`)
before reaching the agent:

- Sender identity is taken from the channel server (e.g. Telegram's numeric user
  id), not from anything the sender can set in a message body.
- The gate is **fail-closed**. The default policy for unknown senders is
  `PolicyStrict` (deny). The alternatives are explicit opt-ins: `PolicyPublic`
  (allow anyone) or `PolicyRequestApproval` (hold the message, notify the owner,
  who must `/approve`). Approval is owner/admin only.
- An unwired conversation is dropped unless owner auto-wire is explicitly enabled.
- There is no path where an unauthorized sender's message reaches the agent
  without an explicit allow or a completed approval.

## Filesystem and data isolation

- **Session keys** derive from external chat input (`channel:chatID`) but are
  sanitized to a single safe path segment (alphanumeric, `-`, `_`, `.`; never
  `.`/`..`) before becoming a filesystem path, and parameterized in SQL. A
  malicious chat id cannot traverse the filesystem or collide with another group's
  data.
- **Mount allowlist** (`internal/mounts`): any extra group mount is validated
  against an external allowlist that **fails closed** when absent (`ErrNoAllowlist`).
  Host paths are symlink-resolved before the allowlist check (no symlink escape),
  rejected if they still contain `..`, and the container path is rejected if it is
  non-absolute or contains a colon (no `-v host:container:opts` injection). RW
  mounts get `:Z`, RO mounts `:ro,Z` (SELinux private relabel).
- **SQL** is parameterized throughout; no user-derived value is concatenated into
  a query.
- The two-DB-per-session boundary keeps writers separate: the host owns
  `inbound.db`, the runner owns `outbound.db` (brief §5.1), so neither side can
  clobber the other across the mount.

## Secrets in the repo

- `.env` and `data/` are gitignored, so tokens, the central DB, the proxy CA
  private key, and conversation transcripts are never committed.
- `.env.example` carries only blank values, safe defaults, and placeholders.
- Vault credential notes record where a secret lives, never the secret itself.

## Residual risks (accepted)

- **Host compromise.** The host holds the decrypted tokens in memory and the
  encryption / CA keys. A full host compromise exposes everything; this is the
  trust boundary, by design.
- **At-rest key locality.** The credential-store encryption key lives in the
  environment, not the data dir, so a stolen data dir / DB dump alone does not
  decrypt the tokens. But the key and data share the host trust boundary; this is
  the standard "encrypt at rest with a local key" posture, not HSM/KMS separation.
- **Decrypted token in host memory.** Tokens are held as Go strings and not
  explicitly zeroed after use. This only matters under host-memory compromise,
  which already defeats the model, so it is not mitigated.
- **Credentials for attacker-controlled hosts.** If an operator deliberately
  stores (`goclaw auth add`) a credential whose target host is controlled by an
  attacker, that host could capture the injected token. Only add credentials for
  hosts you trust.
- **Container capability posture.** The container is non-root (`--user 1000:1000`)
  and rootless, but runs with Podman's DEFAULT capability set: goclaw does not add
  `--cap-drop=ALL`, `--security-opt=no-new-privileges`, a custom seccomp profile, or
  a read-only rootfs. The boundary is "rootless, non-root, explicit mounts", which is
  strong, but it is not hardened to the minimum-capability floor a defense-in-depth
  pass would add. Tightening this (drop-all-caps + no-new-privileges, then add back
  only what the runner needs) is a known follow-up; a container escape is already out
  of scope (see the note under the plugin install section).
