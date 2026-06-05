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
   The scan also rejects source that references goclaw's host/agent CREDENTIAL env
   var names (`ANTHROPIC_API_KEY`, `GH_TOKEN`, `GOCLAW_GITHUB_TOKEN`,
   `CLAUDE_CODE_OAUTH_TOKEN`, `GOCLAW_SECRET_ENCRYPTION_KEY`), and the distinctive
   fragments those names split into, to catch a plugin reading them (including a
   `"ANTHROPIC" + "_API_KEY"` style concatenation). IMPORTANT, read honestly: this
   is a BEST-EFFORT DETERRENT, not a guarantee. A determined plugin can assemble the
   var name at runtime (mid-word splits, base64, a value fetched at runtime) and slip
   past any static grep; we do some looking, but a plugin is open-source code you
   chose to install, and the source is public, so it is easy to work around. The
   final responsibility is the operator's: install plugins you trust. The REAL
   protections that do not depend on the scan are (a) the env allowlist, a plugin is
   never handed these vars in the first place (see "Plugins run in the sandbox"), and
   (b) the credential proxy, the container holds only placeholders. The scan is a
   third, softer layer on top of those two.
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

### How interception works (and how HTTPS is "maintained")

A natural question: if the proxy intercepts TLS, how is the connection still HTTPS once
the cert is reauthored by the proxy? The framing has a hidden assumption worth correcting
first: **the upstream's real cert is never intercepted or rewritten. It is never even seen
by the client.** The proxy TERMINATES one TLS session and ORIGINATES a second, separate
one. "Maintaining HTTPS" does not mean preserving the original cert end to end; it means
each of the two legs is independently a valid, encrypted, authenticated TLS session.

Step by step, for `https://api.anthropic.com`:

1. **The client opts into the proxy.** The container's env has
   `HTTPS_PROXY=http://host.docker.internal:<port>`, so the agent's tooling does NOT dial
   Anthropic. It sends the proxy an HTTP `CONNECT api.anthropic.com:443`. The proxy
   hijacks that raw TCP connection, replies `200 Connection Established`, and the client,
   believing it has a clean tunnel, starts a TLS handshake INTO that tunnel.
2. **The proxy terminates the client's TLS with a freshly MINTED leaf, not Anthropic's
   cert.** On the fly it generates a new certificate that CLAIMS to be `api.anthropic.com`:
   a new ECDSA P-256 key, `CommonName`/SAN = the requested host (no wildcards), 24h
   validity, cached per host, and **signed by goclaw's own CA private key** (with the CA
   cert appended to the chain). `tls.Server(client, leafCfg)` then completes a handshake
   AS IF it were Anthropic. The client's TLS now terminates at the proxy, with a key the
   proxy holds, so the proxy can read the plaintext.
3. **The client accepts that cert only because goclaw's CA is trusted IN THE CONTAINER.**
   A normal client would reject it (the CA is in no public root store). It validates only
   because goclaw's CA cert is mounted read-only into the container's trust store
   (`NODE_EXTRA_CA_CERTS` / `SSL_CERT_FILE` / `GIT_SSL_CAINFO` point at it). So inside the
   container, and only there, a leaf goclaw signed for `api.anthropic.com` chains to a
   trusted root and the name matches, so the handshake succeeds.
4. **The proxy opens a SECOND, independent TLS session to the real Anthropic.** Holding
   the plaintext request, it injects the real `Authorization` header (the token the
   container never has) and forwards over a fresh TLS connection to the actual
   `api.anthropic.com`, validated against the SYSTEM root store, Anthropic's real cert,
   verified normally. Here the proxy is just an ordinary HTTPS client.

The topology is two TLS sessions spliced at the proxy, not one passed through:

```text
container client ──TLS #1──> proxy ──TLS #2──> api.anthropic.com
  trusts goclaw CA          (plaintext          validates Anthropic's
  cert says "anthropic"      visible here;        real cert vs the
  signed by goclaw CA        token injected)      system root store
```

So is HTTPS maintained? **Yes, but not end to end.** There is a point, inside the proxy,
on the host, where the bytes are plaintext, and that is the POINT, not a flaw: the proxy
must see plaintext to inject the credential the container is not allowed to hold. What is
maintained is that each leg is a real, validated TLS session (leg 1 against goclaw's CA we
deliberately trust in-container; leg 2 against Anthropic's real cert via system roots), so
nothing speaks unencrypted over the network. This is the SAME mechanism as a generic MITM
attack; what makes it safe rather than an attack is the trust scoping below, the forging
CA is trusted ONLY inside the sandbox, nowhere else.

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

## Tradeoffs: what each control buys, and where it may be too much

Security has a cost (complexity, bugs, friction), so it is worth being honest about
which controls are load-bearing and which are belt-and-suspenders for a self-hosted,
single-user deployment. Host compromise is explicitly OUT of scope here (if the host is
owned, everything is); this section judges each control on what it buys ASSUMING a
trusted host. The controls separate into two layers, and they are independent: you can
keep one and drop the other.

**Agent containment (load-bearing, keep it).** These defend against the defining risk of
giving an LLM tools and a network: a PROMPT-INJECTED AGENT TURNED AGAINST YOU. A message,
a cloned repo's README, or a web page can steer the agent into running commands you did
not intend. This is a live, common risk, not paranoia, and it has nothing to do with
machine compromise.

- The **container** is the one boundary that contains an injected agent. The agent runs
  arbitrary bash and clones arbitrary repos (prime injection vectors); without the
  sandbox, a successful injection runs on the host as you. This is the single most
  justified control in the system.
- The **access gate** (fail-closed) is about the OPEN INTERNET reaching your bot. Anyone
  can message a channel the bot is on; without the gate a stranger drives your agent (your
  tokens, your tools, your money). Cheap and essential.
- The **mount allowlist** keeps the agent from seeing arbitrary host paths. Cheap, fail
  -closed, directly relevant.
- **Identity namespacing** stops one channel spoofing another's owner at the gate. Tiny,
  real.

Dropping any of these would be a genuine mistake, even for a single user, because the
threat they address (a hostile message turning your own agent against you) is exactly the
threat a self-hosted agent faces every day.

**Credential protection (optional, judge for your deployment).** This layer protects the
KEY MATERIAL: the credential proxy, the plugin secret-read scan, and the sandboxed plugin
build. The honest read: for a single user who chose their own plugins and trusts their
host, this layer defends a NARROWER scenario at real cost.

- The **credential proxy** (TLS-intercepting MITM, per-host CA, leaf minting) stops a
  prompt-injected agent from reading the literal token (`echo $ANTHROPIC_API_KEY` returns
  `placeholder`). But the agent can USE the credential regardless (it makes API calls on
  your dime whether or not it can read the key), so the proxy's real win is narrow:
  preventing EXFILTRATION of the raw token to a third party who then uses it elsewhere.
  That is a real risk, but it is a lot of TLS-interception machinery (and a class of
  subtle bugs, e.g. an occasional `bad record MAC`) for that one scenario. The simpler
  fallback, a direct env key, is fully supported; with it the agent holds the real token.
  Using the proxy is the more secure choice, but NOT using it (and accepting the agent
  holds the key) is a defensible call for a personal deployment.
- The **plugin secret-read scan** is best-effort and trivially evadable (see "Installing a
  plugin"). The real protection is the env ALLOWLIST (the plugin never receives the
  secret var). The scan is defense-in-depth bordering on ceremony when you vet what you
  install.
- The **sandboxed in-container plugin BUILD** prevents a malicious plugin's build-time
  code (`go:generate`, cgo) from running on the host. Good engineering, but you install
  plugins by a git URL you typed; for a vetted-input single-user tool it is belt-and
  -suspenders.

So a fair summary of the "is this too much?" critique: it lands on the CREDENTIAL layer
(elaborate machinery for a narrow exfiltration win on a trusted single-user host), and it
does NOT land on the CONTAINMENT layer (which protects against an agent turned against
you, a real and constant risk). The two are separable: the credential proxy is a config
choice (`GOCLAW_*` env / `goclaw auth`); the container, gate, and mounts are not optional.

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
