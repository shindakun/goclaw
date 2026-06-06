# Credential proxy: OAuth token refresh

Status: DESIGN. This extends the credential proxy from "inject a STATIC token per host"
to "inject a short-lived OAuth ACCESS token, refreshed host-side from a stored refresh
token." It is forced by the Gmail channel plugin (`docs/gmail-channel-plugin.md`) but
generalizes to every OAuth upstream (Google APIs, Slack, anything "sign in with X").
Read `docs/security.md` ("Credential proxy") first.

## 1. What the proxy does today, and the exact gap

Today a stored credential is a static secret. The flow:

- `credstore.Credential` = `{ID, Name, TargetURL, TargetHost, encrypted-token}`. The
  token is a fixed string (an API key, a PAT).
- `Store.ResolveByHost(host)` returns that decrypted token.
- The proxy's `injectAuth(req, host, token)` puts it on the request (`x-api-key` for
  Anthropic, Basic for GitHub git, `Authorization: Bearer` otherwise) and forwards.

That works because an API key never changes. **An OAuth access token does: it expires
(~1h) and must be refreshed using a long-lived refresh token.** So a stored static
"token" is the wrong shape: what we must store is the REFRESH token (+ client id/secret
+ token endpoint), and what we must INJECT is a freshly-minted access token. The proxy
has no notion of "mint and refresh" yet. That is the entire gap.

## 2. The design: make RESOLUTION oauth-aware, leave injection alone

The cleanest seam is the one that already exists. `injectAuth` takes a `token string`
and does not care where it came from. So we do NOT touch injection. We change what
RESOLUTION returns: for an OAuth credential, resolution mints/refreshes an access token
and hands that to `injectAuth` as the `token`. Everything downstream is unchanged.

```text
                          before:  ResolveByHost(host) -> static token --> injectAuth
                          after:   ResolveByHost(host) -> {static token | fresh OAuth access token} --> injectAuth
```

### 2a. A credential gains a kind

Extend the stored credential so it is one of two kinds:

- `kind = "static"` (today): the encrypted blob IS the token. Resolution returns it.
- `kind = "oauth2"`: the encrypted blob is an OAuth bundle, NOT a bearer token:
  - `refresh_token` (the long-lived secret, the most sensitive thing here),
  - `client_id`, `client_secret`,
  - `token_url` (e.g. `https://oauth2.googleapis.com/token`),
  - `scopes`,
  - and a CACHED `access_token` + `expires_at` (so we do not refresh every request).

This is additive: existing rows are `kind=static` and behave exactly as now. A migration
adds a `kind` column (default `static`) and widens the encrypted payload from "a token"
to "a token OR a JSON oauth bundle." The encryption (AES-256-GCM, key from
`GOCLAW_SECRET_ENCRYPTION_KEY`) is unchanged; only the plaintext shape differs by kind.

### 2b. Resolution mints/refreshes

`ResolveByHost(host)` for an `oauth2` credential:

1. If a cached `access_token` exists and `expires_at` is comfortably in the future (say
   > 60s margin), return it. No network.
2. Otherwise POST to `token_url` with `grant_type=refresh_token` + `refresh_token` +
   `client_id` + `client_secret`, get back a new `access_token` + `expires_in`.
3. Cache the new `access_token` + computed `expires_at` (encrypted, in the credstore),
   and return the access token.

The refresh POST happens HOST-SIDE, in the proxy, using the host-held refresh token. The
container never sees the refresh token OR the access token: `injectAuth` puts the access
token on the request at the proxy, exactly as it puts a static token today. The
two-session-splice TLS interception (security.md) is unchanged; this only changes how
the `token` string is obtained before injection.

### 2c. Concurrency and the refresh stampede

Multiple in-flight requests to Gmail can hit an expired token at once. Refreshing N times
in parallel is wasteful and some providers rotate/invalidate the refresh token on use, so
a stampede can break auth. So refresh must be SINGLE-FLIGHT per credential: the first
request to find the token expired refreshes; concurrent requests wait for that one
refresh and reuse its result (`golang.org/x/sync/singleflight`, or a per-credential
mutex). This is the one piece of real concurrency design in the extension.

### 2d. Refresh-token rotation

Some providers (Google included, under some configs) return a NEW refresh token on
refresh and invalidate the old one. So resolution must, when the token response carries a
`refresh_token`, PERSIST the rotated refresh token back to the credstore (encrypted),
atomically, or the next refresh fails. Miss this and auth works until the first rotation,
then breaks silently. It must be handled from day one.

## 3. Storing an OAuth credential: the auth command

`goclaw auth add` today takes a host + a token. OAuth needs more, and crucially the
refresh token comes from a browser consent flow, not a paste. Options:

- `goclaw auth add-oauth --host gmail.googleapis.com --client-id ... --client-secret ...
  --token-url ... --scopes ...` then run a one-time local consent flow (open a browser,
  capture the `code` on a localhost redirect, exchange for the initial refresh token),
  and store the bundle. This is a new subcommand; it reuses the credstore encryption.
- Simpler bring-up: accept a refresh token directly if the operator already has one
  (e.g. extracted via `gcloud` or a helper), `goclaw auth add-oauth ... --refresh-token
  <rt>`. Skips the consent-flow UX but still stores the right bundle.

Either way the result is one `kind=oauth2` credstore row, host-keyed like every other
credential, and the proxy picks it up by host automatically.

## 4. Security review

- The MOST sensitive secret (the refresh token) and the client secret live ONLY in the
  host credstore, encrypted at rest, and are used ONLY by the host proxy to mint access
  tokens. They never enter the container. This is STRICTLY BETTER than NanoClaw's Gmail
  (which mounts the live token into the container) and consistent with goclaw's model.
- The container holds a placeholder, same as for Anthropic/GitHub today. A prompt
  -injected agent that runs `env` gets nothing usable.
- The minted access token is short-lived and injected per request at the proxy; it is
  never written anywhere the container can read.
- The access-token CACHE is host-side and encrypted. A stolen data dir / DB dump does not
  decrypt it (the key is env-only, `GOCLAW_SECRET_ENCRYPTION_KEY`), same property as
  today.
- New residual: the host now makes an outbound call to the OAuth `token_url` on behalf of
  the credential. That host is implicitly trusted (it is the identity provider); a
  compromised `token_url` could capture the refresh exchange, so `token_url` must be a
  known provider endpoint, not operator-supplied-arbitrary without thought. Same caution
  as "only add credentials for hosts you trust."

## 5. What changes, concretely

- `internal/credstore`: add a `kind` column (migration, default `static`); a
  `Credential` carries kind; the encrypted payload is a token (static) or an oauth bundle
  (oauth2). Add the rotated-refresh-token persist path.
- `internal/credproxy`: `ResolveByHost` becomes oauth-aware (cache check -> single-flight
  refresh -> persist rotation -> return access token). `injectAuth` UNCHANGED.
- `cmd/goclaw auth`: `add-oauth` subcommand (consent flow or direct refresh-token), and
  `list` shows kind.
- Tests: token-not-expired returns cached (no network); expired triggers exactly one
  refresh under concurrent load (single-flight); a refresh response carrying a new
  refresh token persists it; a static credential still resolves unchanged.

## 6. Scope discipline

This is the FIRST OAuth case (Gmail). Build it for one provider's token endpoint
(Google's `oauth2.googleapis.com/token`, standard RFC 6749 `refresh_token` grant) and
keep the bundle provider-agnostic, but do NOT try to abstract every provider's quirks up
front (PKCE variants, device flow, provider-specific scopes) until a second OAuth
provider actually lands. The refresh grant is standardized enough that Google + a
generic RFC 6749 path covers the common case; specialize when a real second case forces
it, the same discipline the channel-plugin work followed (build IRC concretely, abstract
only when Gmail arrived).
