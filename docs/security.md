# Security model

This documents goclaw's threat model and the controls that enforce it. It is the
reference for "what is goclaw trying to protect against, and how."

## Threat model

Two things are treated as untrusted:

1. **Inbound chat input.** Anyone can message a channel the bot is on. Sender
   identity, message content, and chat ids are attacker-controlled.
2. **The agent itself.** The Claude agent runs arbitrary tools (bash, git, clones
   of third-party repos) and can be steered by prompt injection from a message or
   a cloned repo. So the agent is assumed potentially hostile, and the container
   is the security boundary.

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
