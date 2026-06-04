# TLS-Intercepting Credential Proxy: Scope

Status: scoping only, nothing built. This replaces the current Anthropic
base-URL proxy with a single TLS-intercepting proxy that also covers GitHub
(`git`/`gh`), which a base-URL redirect cannot reach.

## Why

The shipped credential proxy (brief §8) keeps the raw Anthropic key out of the
container by pointing the `claude` CLI at a plain-HTTP local endpoint via
`ANTHROPIC_BASE_URL`. That trick only works for tools that honor a base-URL
override. `git` and `gh` go straight to `github.com` over TLS with no override,
so the `GH_TOKEN` is still passed into the container raw. To inject a credential
into their traffic, the proxy must terminate (intercept) their outbound TLS.

## Decisions (locked)

1. **Replace, not alongside.** Route Anthropic through the same TLS-intercepting
   proxy and remove the `ANTHROPIC_BASE_URL` special-case. One mechanism, uniform
   TLS. (Risk: reworks a path already verified in production. See migration.)
2. **CA key location: env-first, else auto-generate a 0600 data-dir file.**
   `GOCLAW_PROXY_CA_KEY` / `GOCLAW_PROXY_CA_CERT` override; otherwise generate and
   persist `{data_dir}/proxy/ca.key` (0600) + `ca.pem`, reused across restarts.
3. **Uncredentialed hosts: blind tunnel.** Only intercept hosts that have a
   stored credential. Everything else is piped through opaquely (no decryption,
   no injection), so the agent's other HTTPS stays end-to-end encrypted to its
   real destination and we never see it.
4. **Tool coverage: `claude` + `git` + `gh` + `curl`.** Each wired via its trust
   env and verified end to end through the proxy before "done".

## How it works (standard MITM proxy)

1. Container gets `HTTPS_PROXY=http://host.docker.internal:<port>` (plus
   `NODE_USE_ENV_PROXY=1` so the Node-based `claude` CLI honors it).
2. For `https://github.com/...`, the container's client sends
   `CONNECT github.com:443` to the proxy.
3. Proxy decides intercept (we have a credential for that host) or blind tunnel.
4. Intercept: respond `200 Connection Established`, hijack the connection, run
   `tls.Server` using a leaf cert minted for `github.com` and signed by our CA
   (which the container trusts), read the decrypted request, inject the auth
   header, dial the real upstream with a normal `tls.Config`, forward, and stream
   the response back through the terminated connection.
5. Blind tunnel: `io.Copy` both directions, no decryption.

Anthropic ends up on the same path, so its inbound hop becomes TLS too (today it
is plain-HTTP-local carrying only a placeholder).

## Components

### internal/credproxy/ca.go (new, ~150 lines, security-sensitive)
- Load CA from env (`GOCLAW_PROXY_CA_KEY`/`_CERT`) or auto-generate (ECDSA P-256),
  persist to `{data_dir}/proxy/ca.{key,pem}` with the key file at 0600.
- Mint per-host leaf certs on demand (`crypto/x509.CreateCertificate`), correct
  SAN = the host, short validity, key usage = server auth, cached per host with
  refresh-before-expiry.
- Expose the CA cert PEM (the host writes it to a file mounted into the container).

### internal/credproxy/credproxy.go (extend, ~200 new lines)
- Switch the listener from a pure HTTP reverse-proxy to one that special-cases
  `CONNECT`: `r.Method == http.MethodConnect` -> `http.Hijacker` to take the raw
  conn.
- Resolve host against `credstore.Hosts()`: intercept vs blind tunnel.
- Intercept: `tls.Server(conn, leafConfig)`, read the request, reuse the existing
  `injectAuth` (api.anthropic.com -> x-api-key, else Authorization: Bearer) and
  SSE-safe streaming, dial upstream over real TLS.
- Keep the current resolver/injection/streaming logic; the CONNECT + TLS-termination
  layer is the new part.

### cmd/goclaw spawn wiring (~40 lines + Containerfile)
- Set `HTTPS_PROXY`/`HTTP_PROXY`=the proxy, `NODE_USE_ENV_PROXY=1`,
  `NO_PROXY`=host.docker.internal (so the CONNECT to the proxy itself is direct).
- Write the CA pem to a host file, mount RO into the container, set trust envs:
  `NODE_EXTRA_CA_CERTS` (claude), `GIT_SSL_CAINFO` (git), `SSL_CERT_FILE` /
  `SSL_CERT_DIR` (curl, Go, python). `gh` uses git's HTTPS stack.
- Drop `ANTHROPIC_BASE_URL`, the placeholder `ANTHROPIC_API_KEY`, and the raw
  `GH_TOKEN`. Anthropic + GitHub tokens now come from the store, injected by the
  proxy.

### internal/credstore (unchanged)
- Already host-matched and encrypted. Just store a GitHub credential too:
  `goclaw auth add github https://api.github.com <token>`. The proxy resolves it
  by host. For `git clone` over `github.com` (not `api.github.com`), note the host
  is `github.com`; store the credential under the host(s) git actually hits.

### config
- `GOCLAW_PROXY_CA_KEY` / `GOCLAW_PROXY_CA_CERT` (override), else the data-dir file.

## Gaps and open questions (found during scoping)

1. **CRITICAL, unverified: does the `claude` CLI route through `HTTPS_PROXY`?**
   We verified the CLI honors `ANTHROPIC_BASE_URL` (the current mechanism). We
   have NOT verified it obeys `HTTPS_PROXY` + `NODE_USE_ENV_PROXY=1` to send
   `api.anthropic.com` through a CONNECT proxy. If it does not, "replace" breaks
   Anthropic and we must keep the base-URL path for Anthropic (i.e. fall back to
   "alongside"). **Verify this FIRST, before building anything**, with a probe:
   run the CLI with `HTTPS_PROXY` pointed at a logging CONNECT listener and a
   trusted CA, confirm it issues `CONNECT api.anthropic.com:443`.
2. **GitHub uses two hosts.** `gh` API calls hit `api.github.com`; `git clone`
   hits `github.com` (and Git LFS / `codeload.github.com`). A single credential
   for one host will not cover both. Decide: store the token under multiple hosts,
   or match by suffix (`*.github.com` + `github.com`). The injection header for
   git's HTTPS is also not a simple `Authorization: Bearer` in all cases (git uses
   Basic with the token as password for some flows). **Needs its own verification**
   that an injected header actually authenticates a real `git clone` / `gh`.
3. **`http.Hijacker` + `http.Server` interplay.** Standard but must be done right:
   after hijack we own the conn; the `http.Server`'s read/write timeouts and
   graceful shutdown no longer apply to it. Need explicit conn deadlines and to
   account for hijacked conns in shutdown.
4. **NO_PROXY for the proxy host itself.** The container must reach
   `host.docker.internal:<port>` directly (that is the proxy). It must be in
   `NO_PROXY` or the client will try to CONNECT to the proxy through the proxy.
5. **Leaf cert SAN/IP correctness.** `git`/`gh`/curl validate hostname against the
   leaf SAN strictly. The leaf must carry the exact requested host as a DNS SAN.
   Wildcards and IP literals need care. This is the most common silent-failure
   source.
6. **System-trust fallback option.** node:22-slim has `update-ca-certificates`
   and the Debian CA dir. If per-tool trust envs prove flaky, we can instead append
   the CA to the system bundle at startup (a Containerfile/entrypoint step). Keep
   this as a fallback; the per-tool env approach is the first target (decided).
7. **CA private key is now a sensitive host artifact.** Whoever holds it can MITM
   the container. Mitigations: 0600, out of the data dir if env-provided, the CA is
   only trusted inside the container (the sandbox), short leaf validity. Document
   that rotating it means re-trusting in the container (handled automatically since
   the host writes the pem each spawn).
8. **HTTP/2.** The `claude` CLI / Anthropic may negotiate HTTP/2 over the
   intercepted TLS. `tls.Server` + `httputil.ReverseProxy` handle h2 via ALPN, but
   the leaf config must advertise the right ALPN protocols, and forwarding must not
   downgrade SSE. Verify streaming still works post-interception.
9. **Migration safety (the real risk of "replace").** Anthropic works in
   production today on the base-URL path. Build + fully verify the MITM path for
   GitHub first, leaving Anthropic on base-URL; only cut Anthropic over once the
   MITM path is proven, with a verify step (a real chat answers) before deleting
   the base-URL code. Never have a window where Anthropic is broken.
10. **Image rebuild required.** Unlike the base-URL proxy (host-only), this needs
    a Containerfile change (CA mount point / trust dir), so it rebuilds the image.

## Build sequence

0. **Verify gap #1** (claude CLI honors HTTPS_PROXY). If it fails, revisit
   "replace" vs "alongside" before proceeding.
1. `ca.go`: CA load/generate + leaf minting + cache, with unit tests (leaf chains
   to CA; a `tls.Client` trusting the CA accepts the leaf for the right host).
2. CONNECT handler + intercept/blind decision + MITM tunnel, tested against a fake
   HTTPS upstream (assert the injected header reaches it; blind tunnel passes bytes
   untouched).
3. Verify gap #2 (real `git clone` / `gh` authenticate with the injected header).
4. Spawn wiring + Containerfile trust dir; rebuild image.
5. Live verification: `claude`, `git clone`, `gh`, `curl` all work through the
   proxy; container env has no real `sk-ant` / GitHub token; injected tokens reach
   the upstreams.
6. Cut Anthropic over from base-URL to the MITM path; verify a real chat; remove
   the base-URL code.
7. Docs (README credential-proxy section, brief §8.1, .env.example, config table).

## Effort and risk

A few days, dominated by CA/leaf correctness, per-tool trust wiring, and end-to-end
verification per tool. Security-sensitive and silent-failure-prone (wrong SAN,
missed trust env, broken streaming). Heavy on verification, light on line count.
Do it as its own focused session with the host stopped for the spawn-wiring and
cutover steps.
