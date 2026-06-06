# Authenticated upstream credentials (OAuth2, DPoP, and beyond)

Status: DESIGN. Adds support for credentials that are not a static string: an OAuth2
Bearer refreshed from a refresh token (Gmail), and atproto/Bluesky DPoP (a per-session
key that SIGNS each request, with a rotating nonce). Read `docs/security.md` ("Credential
proxy") first.

This doc was rewritten after Bluesky was used to stress-test an earlier Google-shaped
draft. That draft abstracted at "resolution returns a token string", which Bluesky
breaks. The corrected abstraction and the constraints are the substance here. It does NOT
decide where the provider logic lives (host vs plugin) or how the credential reaches the
request, those forks are laid out in sections 5 and 6 for a decision.

## 1. Why the obvious abstraction is wrong

The tempting design: a credential resolves to a token string; a single `injectAuth(req,
host, token)` puts `Authorization: Bearer <token>` (or `x-api-key`) on the request. That
is what the static-key path does today and it is fine for API keys and even for OAuth2
Bearer. It is also a trap, because it bakes in three assumptions that are Google-shaped,
not universal:

1. **Auth is a STRING on a header.** Bluesky's DPoP is not: each request carries
   `Authorization: DPoP <token>` PLUS a `DPoP:` header holding a JWT signed with a
   per-session PRIVATE KEY, and that JWT embeds a server-issued NONCE that rotates on
   nearly every response. The token alone is useless; the request must be SIGNED, per
   request, with a key, using the current nonce. No string-on-a-header model expresses
   this. (See the sibling `bskyoauth` repo: DPoP is an `http.RoundTripper` that signs
   each request and tracks the nonce.)
2. **The endpoints are fixed and known.** With Google, `token_url` is a constant
   (`oauth2.googleapis.com/token`). With atproto, the token endpoint is PER-USER: it is
   discovered by resolving the user's handle (`@alice.bsky.social` -> DID -> PDS -> that
   PDS's OAuth metadata). "A known provider endpoint" is not a thing for atproto.
3. **The credential is one secret (a token / refresh token).** atproto's credential is a
   BUNDLE that includes a private signing KEY (`DPoPKey`), a `DID`, an `AccessToken`, a
   `RefreshToken`, AND a live `DPoPNonce`. The "store one encrypted token" shape has no
   room for a key, and no notion that auth requires SIGNING, not presenting.

So the line was drawn one level too low. The thing that varies per provider is not "what
token string do I return", it is "HOW DO I AUTHENTICATE AN OUTBOUND REQUEST to this
upstream", which spans credential shape, refresh mechanics, header(s), signing, and
endpoint discovery. The abstraction must sit there.

## 2. The corrected abstraction: authenticate a request

The generic contract is not `Token(host) -> string`. It is closer to:

```text
Authenticator.Authenticate(req *http.Request) error   // make this request authenticated
Authenticator.Refresh(ctx) error                      // renew whatever expires
```

An `Authenticator` holds a provider-typed credential and knows how to turn a bare request
into an authenticated one. Two concrete shapes the design must fit TODAY (one built, one
target) prove it is wide enough:

- **OAuth2 Bearer (Gmail):** the credential is `{refresh_token, client_id, client_secret,
  token_url, scopes, cached access_token + expires_at}`. `Authenticate` sets
  `Authorization: Bearer <access_token>`, refreshing first if expired. Stateless per
  request beyond the cached token.
- **atproto DPoP (Bluesky):** the credential is `{did, pds/token endpoint, refresh_token,
  access_token, DPoP private key, current nonce}`. `Authenticate` SIGNS the request: sets
  `Authorization: DPoP <access_token>` and a `DPoP:` JWT signed with the key over the
  request + current nonce, and UPDATES the nonce from the response. `Refresh` runs the
  atproto refresh grant (itself DPoP-signed). This is exactly what `bskyoauth`'s
  RoundTripper already does; the design must let that drop in, not fight it.

Note the atproto case is naturally an `http.RoundTripper` (it signs per request and reads
the response nonce). So `Authenticator` may be better expressed AS a RoundTripper
factory: "give me a transport that authenticates requests to this upstream." The Bearer
case is a trivial RoundTripper (add a header); the DPoP case is `bskyoauth`'s. Picking
RoundTripper vs an `Authenticate(req)` method is a build detail; the POINT is the unit is
"authenticate a request", not "a token".

## 3. What credstore stores: an opaque, provider-typed credential

credstore stops storing "a token" and stores a provider-typed, opaque, encrypted blob:

- `kind` names the scheme, not just static-vs-oauth: e.g. `static`, `oauth2-bearer`,
  `atproto-dpop`, with room for more. (Today everything is `static`.)
- The encrypted payload shape is DEFINED PER KIND. `static` = the token. `oauth2-bearer`
  = the Google-style bundle (section 2). `atproto-dpop` = the bskyoauth session bundle
  INCLUDING the DPoP private key and the live nonce.
- The encryption (AES-256-GCM, key from `GOCLAW_SECRET_ENCRYPTION_KEY`) is unchanged;
  only the plaintext shape differs by kind. credstore treats the blob as opaque bytes
  per kind, it does not parse provider internals; the provider's Authenticator does.

This is additive (existing rows are `static`) and, crucially, it does NOT commit us to
implementing atproto now. It commits us to a SCHEMA that can hold atproto's bundle
(key + nonce, not just a token) so adding it later is a new kind, not a migration of the
abstraction. That is the whole point of stress-testing with Bluesky before building.

### 3a. Mutability: the credential is read-WRITE, and that is new

A static token is write-once, read-many. An authenticated credential MUTATES as it is
used: the OAuth2 access-token cache updates on refresh; atproto's nonce updates on nearly
EVERY request, and the refresh token can rotate. So credstore must support updating the
stored blob, with single-flight on refresh (section 4), and the consuming layer must
write the mutated credential back. This read-write, per-request-mutating nature is a
bigger change than "add a column", and it is the same whether the consumer is the host or
the plugin (section 5).

## 4. Refresh / rotation / nonce: the lifecycle, generically

Regardless of provider, three lifecycle concerns recur, and credstore (or the
Authenticator over it) must handle all three:

- **Refresh when expired.** OAuth2: POST the refresh grant when the cached access token is
  within ~60s of expiry. atproto: same idea, its own refresh grant (DPoP-signed).
- **Single-flight.** Concurrent requests hitting an expired token must trigger ONE
  refresh, not N (wasteful, and some providers invalidate the refresh token on use). Per
  -credential single-flight (`golang.org/x/sync/singleflight` or a mutex). Lives wherever
  the Authenticator lives.
- **Rotation / nonce persistence.** A refresh response may carry a NEW refresh token
  (Google can; atproto does) that invalidates the old, and atproto hands a new nonce on
  most responses. Both MUST be persisted back to the stored credential atomically, or the
  next call fails. Miss this and auth works until the first rotation, then breaks
  silently. The nonce case makes this acute: it is not a once-an-hour event, it is
  every request, so the write-back path must be cheap and correct.

## 5. OPEN FORK A: where does the provider logic live (host vs plugin)?

The Authenticator (refresh, signing, nonce, discovery) is provider-specific code. Two
homes, and Bluesky/DPoP makes the choice consequential because DPoP needs a SIGNING KEY
used PER REQUEST:

- **A1. Host-side Authenticator.** The Authenticator lives in goclaw; the credential
  (incl. the DPoP key) stays host-side, encrypted; the host authenticates outbound
  requests on the plugin's behalf. PRO: the most sensitive secret (refresh token, DPoP
  private key) never enters the container; consistent with goclaw's model; one place per
  provider. CON: the credential proxy / delivery layer gains a per-provider hook, and for
  DPoP that means per-request SIGNING happening host-side at the proxy, real
  provider-specific logic in the proxy path, not just "inject a string".
- **A2. In the plugin via a kit helper.** goclawkit ships the provider auth (for atproto,
  literally `bskyoauth`'s RoundTripper); the plugin holds the credential and signs. PRO:
  the host stays provider-agnostic (it just delivers a credential blob in); matches how
  `bskyoauth` already works (a transport the client wraps); no DPoP logic in the proxy.
  CON: the credential (DPoP key, tokens) lives IN the container, the weaker posture, and
  it re-couples to "secret in the sandbox", the thing the proxy exists to avoid.

This is the real architectural fork and it interacts with section 6. Note it is NOT
all-or-nothing per provider: one could put OAuth2-Bearer host-side (A1, since the proxy
can already inject a Bearer) and atproto-DPoP in the plugin (A2, since per-request signing
in the proxy is heavy). A hybrid is legitimate and may be the honest answer.

## 6. OPEN FORK B: how the credential reaches the request (delivery)

Orthogonal to fork A: once an authenticated request exists, by what path does the
credential get applied? This is the "proxy is opt-in" tension from before, now sharper:

- **Proxy injects.** Works cleanly for Bearer (set a header). For DPoP it means the proxy
  holds the key and signs per request, i.e. fork A1 with the proxy as the Authenticator's
  home. Heavy for DPoP.
- **Token/credential file or env to the plugin.** The host refreshes and hands the
  current credential to the plugin (file the plugin re-reads, or the GMAIL_BEARER-style
  env for Bearer). For DPoP this is fork A2 (the plugin signs). Works with the proxy off;
  weaker (credential in container).

Forks A and B are entangled: "proxy injects DPoP" forces host-side signing; "plugin
signs" forces credential-in-container. The clean combinations are:
(host Authenticator + proxy-inject) for Bearer, and (plugin helper + credential-in) for
DPoP. Whether to support both, or force one, is the decision.

## 7. Getting the initial credential: the consent flow (and it varies too)

The one-time bootstrap also is not uniform:

- **OAuth2 / Google:** Authorization Code grant. Operator creates a Desktop OAuth client
  in GCP, downloads client id/secret. goclaw opens the browser to the auth URL with a
  `redirect_uri=http://127.0.0.1:<ephemeral>`, runs a SHORT-LIVED loopback HTTP server in
  the `goclaw auth add-oauth` command (NOT the running daemon) to catch the `?code=`,
  exchanges it for the refresh token, stores the bundle. Headless fallback: print the URL,
  operator pastes the `code` back (no server), or `--refresh-token <rt>` if they already
  have one.
- **atproto / Bluesky:** different. The client is identified by a published
  `client_metadata` URL (not a client_id/secret), the flow uses PAR (Pushed Authorization
  Requests), endpoints are resolved from the user's handle, and DPoP is in play from the
  authorization request onward. `bskyoauth` already implements this; the consent command
  would drive it.

So the consent command is ALSO provider-typed: `goclaw auth add-oauth --provider google
...` vs `--provider atproto --handle @me.bsky.social ...`. Common scaffolding (the
loopback callback server, the browser-open, the headless paste fallback) can be shared;
the per-provider request construction cannot. Key reassurance: the loopback callback
server is a ONE-TIME thing inside the `auth` command, never the running host; the daemon
only ever refreshes (a backchannel call, no browser, no server).

## 8. What this means for building now (scope discipline)

Build Gmail (OAuth2-Bearer) CONCRETELY, but behind the section-2 abstraction, not the
"return a token" one, so atproto can slot in as a new kind + a new Authenticator without
reworking the seam. Specifically, what to do now and what to merely NOT preclude:

DO now (for Gmail):

- credstore: provider-typed `kind` (`static`, `oauth2-bearer`), opaque per-kind payload,
  READ-WRITE credential with single-flight refresh and rotation persist (section 4).
- An `Authenticator` (or authenticating RoundTripper) for `oauth2-bearer`.
- `goclaw auth add-oauth --provider google` with the loopback-callback consent + headless
  fallbacks (section 7).
- Decide forks A and B for the Bearer case only (likely: host Authenticator + proxy
  inject, since the proxy can already set a Bearer).

DO NOT preclude (for atproto later):

- The `kind` enum and the opaque per-kind payload must be able to hold a bundle with a
  PRIVATE KEY and a per-request NONCE (atproto-dpop), not just tokens.
- The consuming contract must be "authenticate a request" (so DPoP signing fits), not
  "give me a token".
- The Authenticator home (fork A) must be able to differ per provider (host for Bearer,
  possibly plugin for DPoP), i.e. do not hardcode "all auth happens at the proxy".

This is the same discipline as the channel work: build the concrete case (IRC, then
Gmail), let the SECOND real case (Gmail for channels, atproto for auth) define where the
abstraction line goes, and do not over-abstract beyond the two cases in hand.

## 9. Decision needed

1. **Fork A (auth-logic home):** host Authenticator (secrets stay host-side, proxy gains
   per-provider hooks) vs plugin helper (host stays dumb, secret in container) vs hybrid
   (host for Bearer, plugin for DPoP).
2. **Fork B (delivery):** proxy-inject vs credential-to-plugin vs both. Entangled with A.
3. For Gmail right now, the low-risk default is host Authenticator + proxy-inject for
   Bearer; the open part is whether to commit to that being the ONLY model or to design
   for the hybrid that atproto-DPoP probably wants.
