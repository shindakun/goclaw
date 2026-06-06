# OAuth credentials: token lifecycle in credstore

Status: DESIGN. Adds support for OAuth2 credentials (a refresh token that mints
short-lived access tokens) to goclaw. Forced by the Gmail channel plugin
(`docs/gmail-channel-plugin.md`) but generalizes to every OAuth upstream (Google APIs,
Slack, anything "sign in with X"). Read `docs/security.md` ("Credential proxy") first.

This doc covers the REFRESH ENGINE: where it lives and how it works. It deliberately does
NOT decide how a fresh access token reaches the plugin (proxy injection vs a mounted
file). That DELIVERY question is open and tracked in section 6, because it interacts with
the proxy being opt-in.

## 1. The key separation: lifecycle is NOT the proxy

The first design of this lived inside the credential proxy (refresh happened in the
proxy's `ResolveByHost`). That was wrong, and here is the precise reason: it coupled an
ORTHOGONAL concern to an OPTIONAL one.

- **Token LIFECYCLE** ("this credential expires and must be minted from a refresh token")
  is a property of the CREDENTIAL. Anyone using an OAuth credential needs it, whether or
  not they run the proxy.
- **Token CONFINEMENT** ("the real token never enters the container") is the PROXY's job,
  and the proxy is opt-in (see security.md "Tradeoffs": containment is mandatory, the
  proxy is a config choice; the direct-env path is supported).

Welding refresh into the proxy means a `.env`-only user with the proxy off CANNOT use an
OAuth credential at all, the refresh logic only runs when the proxy intercepts. That is
backwards: a static API key works fine in `.env` with the proxy off; an OAuth credential
should not be MORE coupled to the proxy than a static key is, it should be at most as
coupled.

So the lifecycle engine lives in **`credstore`**, below both the proxy and the
direct-env path. credstore can MINT A CURRENT ACCESS TOKEN for an OAuth credential,
refreshing when expired. WHO consumes that token (the proxy, injecting per request; or
the host, handing it to the plugin some other way) is a separate layer, section 6.

```text
                    credstore (owns OAuth lifecycle: refresh, rotation, cache)
                          |  AccessToken(host) -> current access token
            +-------------+--------------+
            |                            |
     proxy ON (opt-in):          proxy OFF / direct:
     inject per request          delivery TBD (section 6)
     (token never in container)
```

## 2. What credstore stores: a credential gains a kind

Today a stored credential is a static secret:

- `credstore.Credential` = `{ID, Name, TargetURL, TargetHost, encrypted-token}`; the
  token is a fixed string (API key, PAT). `Store.ResolveByHost(host)` returns it.

Extend it so a credential is one of two kinds:

- `kind = "static"` (today): the encrypted blob IS the token. Resolution returns it.
- `kind = "oauth2"`: the encrypted blob is an OAuth bundle, NOT a bearer token:
  - `refresh_token` (the long-lived secret, the most sensitive thing here),
  - `client_id`, `client_secret`,
  - `token_url` (e.g. `https://oauth2.googleapis.com/token`),
  - `scopes`,
  - a CACHED `access_token` + `expires_at` (so we do not refresh every call).

Additive: existing rows are `kind=static` and behave exactly as now. A migration adds a
`kind` column (default `static`) and widens the encrypted payload from "a token" to "a
token OR a JSON oauth bundle." The encryption (AES-256-GCM, key from
`GOCLAW_SECRET_ENCRYPTION_KEY`) is unchanged; only the plaintext shape differs by kind.

## 3. The refresh engine (in credstore)

A new credstore method, distinct from `ResolveByHost`, that returns a CURRENT access
token for an OAuth credential, refreshing if needed:

```text
AccessToken(host) -> (accessToken string, err error)
```

For an `oauth2` credential:

1. If the cached `access_token` exists and `expires_at` is comfortably in the future
   (say > 60s margin), return it. No network.
2. Otherwise POST to `token_url` with `grant_type=refresh_token` + `refresh_token` +
   `client_id` + `client_secret`; get back a new `access_token` + `expires_in`.
3. Cache the new `access_token` + computed `expires_at` (encrypted, in credstore), and
   return the access token.

`ResolveByHost` (the existing static path) is untouched for `kind=static`. A consumer
that wants "whatever bearer this host needs right now" calls `AccessToken` for oauth2 and
`ResolveByHost` for static, OR we give it a single `BearerForHost(host)` that dispatches
on kind. (Pick during build; the engine is the same either way.)

### 3a. Single-flight (the one real concurrency point)

Multiple in-flight requests can hit an expired token at once. Refreshing N times in
parallel is wasteful, and some providers rotate/invalidate the refresh token on use, so a
stampede can break auth. Refresh MUST be single-flight per credential: the first caller
to find the token expired refreshes; concurrent callers wait for that one refresh and
reuse its result (`golang.org/x/sync/singleflight` keyed by credential id, or a
per-credential mutex). This lives in credstore so BOTH delivery paths get it for free.

### 3b. Refresh-token rotation (handle day one or it breaks silently)

Some providers (Google, under some configs) return a NEW refresh token on refresh and
invalidate the old one. So when the token response carries a `refresh_token`, credstore
MUST persist the rotated refresh token back (encrypted), atomically, or the next refresh
fails. Miss this and auth works until the first rotation, then breaks with no obvious
cause. Not optional.

## 4. Storing an OAuth credential: the auth command

`goclaw auth add` takes a host + a token. OAuth needs more, and the refresh token comes
from a browser consent flow, not a paste:

- `goclaw auth add-oauth --host gmail.googleapis.com --client-id ... --client-secret ...
  --token-url ... --scopes ...` then a one-time local consent flow (open a browser,
  capture the `code` on a localhost redirect, exchange for the initial refresh token),
  storing the bundle. Reuses the credstore encryption.
- Simpler bring-up: accept a refresh token directly if the operator has one (e.g. via
  `gcloud` or a helper): `goclaw auth add-oauth ... --refresh-token <rt>`. Skips the
  consent UX, stores the same bundle.

Either way the result is one `kind=oauth2` credstore row, host-keyed like every other
credential.

## 5. Security review (the engine)

- The MOST sensitive secrets (the refresh token, the client secret) live ONLY in
  credstore, encrypted at rest, and are used ONLY host-side to mint access tokens. With
  the proxy on, they and the access token never enter the container. This is STRICTLY
  BETTER than mounting the live token into the container (NanoClaw's Gmail).
- The access-token cache is host-side and encrypted; a stolen data dir / DB dump does not
  decrypt it (key is env-only, `GOCLAW_SECRET_ENCRYPTION_KEY`), same property as today.
- New residual: the host makes an outbound call to `token_url` to refresh. That host is
  implicitly trusted (it is the identity provider); a compromised `token_url` could
  capture the refresh exchange, so `token_url` must be a known provider endpoint, not
  arbitrary operator input without thought. Same caution as "only add credentials for
  hosts you trust."
- NOTE: the container-side exposure depends on DELIVERY (section 6), not the engine. With
  proxy injection, nothing reaches the container. With a mounted token file, the live
  access token sits in the container (weaker). The engine is identical; the security
  posture is set by the delivery choice.

## 6. OPEN: how the fresh token reaches the plugin (delivery)

This is the part still under discussion, separated out because it is where the
"proxy is opt-in" tension actually lives. The engine (sections 2-4) is the same
regardless; only delivery differs. The crux: OAuth is NOT a "set it in `.env` and forget"
secret like a static key, it needs a LIVE host-side component feeding fresh tokens,
because an access token expires hourly and a running container's env is fixed at launch.
The options:

- **A. Proxy injects (OAuth requires the proxy).** The proxy calls credstore's
  `AccessToken(host)` and injects the bearer per request, exactly as it injects a static
  token today. Token never enters the container. Cost: an OAuth channel implies the proxy
  is on. Static keys stay proxy-optional; OAuth is the documented exception. Least code,
  best security.
- **B. Token file (decouple from the proxy).** A lightweight host refresher writes the
  current access token to a file mounted into the plugin dir; the plugin re-reads it each
  request. Works with the proxy off. Cost: the live token sits in the container (weaker),
  and it is a SECOND credential-delivery mechanism parallel to the proxy.
- **C. Pluggable delivery (both).** The engine is shared; delivery is config: proxy-inject
  OR token-file. Most flexible, most surface.

The engine in this doc is built to serve ANY of these (the delivery layer just calls
`AccessToken`). The decision is deferred to a follow-up; the lifecycle work does not block
on it and is worth doing regardless.

## 7. What changes, concretely (engine only; delivery is section 6)

- `internal/credstore`: migration adds a `kind` column (default `static`); `Credential`
  carries kind; the encrypted payload is a token (static) or an oauth bundle (oauth2). Add
  `AccessToken(host)` (cache check -> single-flight refresh -> persist rotation -> return),
  and the rotated-refresh-token persist path. `ResolveByHost` unchanged for static.
- `cmd/goclaw auth`: `add-oauth` subcommand (consent flow or direct refresh token); `list`
  shows kind.
- Tests: not-expired returns cached (no network); expired triggers exactly one refresh
  under concurrent load (single-flight); a response carrying a new refresh token persists
  it; a static credential resolves unchanged.
- The proxy / delivery wiring is NOT in this step; see section 6.

## 8. Scope discipline

This is the FIRST OAuth case (Gmail). Build it for one provider's token endpoint
(Google's `oauth2.googleapis.com/token`, standard RFC 6749 `refresh_token` grant) and
keep the bundle provider-agnostic, but do NOT abstract every provider's quirks up front
(PKCE variants, device flow, provider-specific scopes) until a second OAuth provider
lands. The refresh grant is standardized enough that Google + a generic RFC 6749 path
covers the common case; specialize when a real second case forces it, the same discipline
the channel-plugin work followed (build IRC concretely, abstract only when Gmail arrived).
