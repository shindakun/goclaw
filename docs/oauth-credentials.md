# Managed upstream credentials (API keys, OAuth2, DPoP, session/connection auth)

Status: OAuth2-Bearer (Gmail) BUILT. The credstore engine (`AddOAuth2` / `AccessToken`
with single-flight refresh + rotation persist), the proxy delivery path (proxy-inject,
re-resolved per request), and the `goclaw auth add-oauth` consent command (refresh-token /
loopback browser / headless paste) are implemented and tested. Forks A and B are DECIDED
for Bearer (host-side authenticator + proxy-inject, sections 8/9). DPoP/atproto and the
connection/session schemes remain DESIGN only; the schema and contract were kept open for
them. Adds support for credentials beyond a static string: OAuth2 Bearer
refreshed from a refresh token (Gmail), atproto/Bluesky DPoP (a per-session key that
SIGNS each request with a rotating nonce), and connection/session schemes (Slack
websocket tokens, WhatsApp session blobs, X browser cookies) that a plugin's own library
consumes. Read `docs/security.md` ("Credential proxy") first. A survey of real schemes is
in section 1.5; it reshaped this doc.

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
token string do I return", it is "HOW DO I AUTHENTICATE to this upstream", which spans
credential shape, refresh mechanics, header(s), signing, endpoint discovery, AND, as the
survey below shows, whether auth is even per-request at all.

## 1.5 A survey of real auth schemes (so we abstract over the real spread)

Before fixing the abstraction, look at what real channel/tool plugins actually use (from
the NanoClaw skills + the sibling `bskyoauth`). The spread is wider than "OAuth refresh",
and it splits along a line that turns out to decide fork A (section 5):

| Scheme | Examples | Credential is | How it is USED | Lifecycle |
|---|---|---|---|---|
| Static API key | Telegram, Discord, OpenAI, Parallel, GitHub PAT | one string | a header / API param | none |
| OAuth2 Bearer | Gmail (Google) | refresh token + client creds + cached access token | `Authorization: Bearer` per request | timed refresh |
| DPoP (signed) | Bluesky (atproto) | token + private KEY + live nonce | per-request SIGNATURE | refresh + per-request nonce |
| Multi-token | Slack Socket Mode | two tokens (`xapp-` opens a websocket, `xoxb-` for API) | one authenticates a CONNECTION, one authenticates requests | static |
| Session blob | WhatsApp (Baileys) | a persisted multi-device SESSION from a QR/pairing handshake | a library reattaches the session; no header at all | re-auth (re-scan) on expiry |
| Browser/cookie | X via Playwright | a browser profile + cookie jar from an interactive login | a real browser carries the session | manual re-login |

The load-bearing distinction is the "How it is USED" column, and it has TWO families:

- **Request-level auth** (static key, OAuth2 Bearer, DPoP): the credential is applied to
  each outbound HTTP request, as a header or a signature. The host CAN do this on the
  plugin's behalf (the proxy injects a Bearer today; it could sign DPoP), so the secret
  can stay out of the container.
- **Connection/session-level auth** (Slack websocket, WhatsApp session, X browser): the
  credential is consumed by a LIBRARY inside the plugin to establish a long-lived
  connection or session; there is no per-request header for the host to inject. The host
  CANNOT be the authenticator here, the plugin's library (Baileys, the Slack SDK,
  Playwright) holds and uses the credential directly.

This is the insight the survey adds: fork A ("host vs plugin auth") is NOT a free choice
we make, it is DETERMINED by the scheme. Request-level schemes permit host-side auth (and
thus secret-out-of-container); connection/session schemes require the credential to live
in the plugin. So goclaw must support BOTH, and the security posture (secret in container
or not) is a function of which scheme, documented per scheme, not a global toggle.

## 1.6 Forward map: future services and the scheme they land in

Which services we might add later, grouped by the scheme they would use, to confirm the
taxonomy covers the future and to show how much one engine buys. The headline: the
OAuth2-Bearer bucket alone is dozens of services and is EXACTLY the Gmail engine, so
building Gmail well unlocks a huge swath of the future for free, the strongest argument
for getting the credstore engine right.

- **OAuth2 Bearer (refresh-token), the big bucket:** the rest of Google (Calendar, Drive,
  Docs, Tasks, YouTube, the MCP servers in play this session are these), Microsoft 365 /
  Graph (Outlook, Teams, OneDrive, a whole second ecosystem), Notion, Linear, Asana,
  Trello, Todoist, ClickUp, Spotify, Reddit, Twitch, GitLab, Dropbox, Zoom, Calendly. All
  are "sign in, get a refresh token, mint Bearer access tokens." They need NOTHING new
  beyond Gmail's engine; the only variation is scopes and (fixed) endpoints.
- **DPoP / signed, currently a bucket of one:** Bluesky / atproto. DPoP is an IETF
  standard (RFC 9449) some OAuth providers are adding as a security upgrade, so this bucket
  may grow, but no other mainstream chat service requires it today.
- **Multi-token / websocket (connection-level):** Slack (have it), Microsoft Teams (bot
  framework), Mattermost / Rocket.Chat / Zulip (self-hosted Slack-likes: a static bot
  token plus an RTM/websocket connection).
- **Session blob (library holds a session):** WhatsApp (have it), Signal (signal-cli /
  libsignal: a device-linked session, very like WhatsApp).
- **Browser / cookie (drive a real browser):** X (have it), LinkedIn, Instagram, Facebook,
  any hostile-API/usable-UI service. Inherently fragile (anti-bot), but a real pattern.

### Three wrinkles that do NOT cleanly fit (so the schema must not preclude them)

The vast majority above fit the six schemes. Three plausible future shapes do not, and
naming them now keeps the schema from being over-fit to "outbound bearer/signature":

1. **Inbound signature verification (the 4b webhook twin).** GitHub / Stripe / Linear
   webhooks, SendGrid inbound: they POST to US, and "auth" is VERIFYING an HMAC signature
   on the INCOMING request (a shared signing secret + timestamp), the mirror of every
   outbound scheme here. This maps onto the channels-design internet-inbound (4b) shape;
   the "credential" is a verification secret, used to validate, not to present. The
   credstore may hold the secret, but the usage is inbound, not request-injection.
2. **Sync-loop / long-poll auth (Matrix, IMAP IDLE).** Matrix uses a static-ish access
   token but the channel is a continuous `/sync` long-poll (not request-response, not a
   websocket); IMAP IDLE is similar. One credential, streaming consumption, a third
   sub-shape between request-level and connection-level. Almost certainly "plugin holds
   it" (connection family), but the consumption model differs.
3. **Local capability, not a credential (iMessage).** iMessage via the macOS Messages DB +
   AppleScript has NO stored secret; it is "this plugin can read chat.db and run AppleScript
   on the host", a host CAPABILITY grant, a mounts/permissions question, not a credstore
   one. It would be a first-party host channel (like the emacs bridge), outside this whole
   design. Good to know it sits outside credstore entirely.

None of these forces a redesign now. They are reasons to keep `kind` open-ended and the
payload opaque-per-kind (sections 3), and to remember that inbound-HMAC and
local-capability may not be credstore's job at all.

## 2. The corrected abstraction: store + manage + deliver a provider-shaped credential

The job of goclaw is therefore NOT "authenticate a request" (too narrow, misses the
connection/session family). It is: **securely STORE a provider-typed credential, MANAGE
its lifecycle (refresh/rotation for the ones that need it), and DELIVER it to wherever it
is consumed, keeping it out of the container WHEN the scheme allows.** The request-level
"authenticate a request" contract below is one consumer of that, the right one for
Bearer/DPoP; the connection/session schemes consume the stored credential differently (it
is handed to the plugin's library).

The request-level contract, for the schemes that have it:

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

## 5. FORK A: where the provider logic lives (mostly DECIDED by the scheme)

The survey (1.5) mostly settles this: where auth logic can live is DETERMINED by whether
the scheme is request-level or connection/session-level, not a free choice.

- **Connection/session schemes (Slack WS, WhatsApp, X): MUST be in the plugin (A2).**
  There is no per-request header for the host to inject; a library inside the plugin
  (Baileys, the Slack SDK, Playwright) consumes the credential to open a connection. The
  host's job for these is only to STORE the credential and DELIVER it into the plugin; the
  secret necessarily lives in the container while in use. No choice.
- **Static key + OAuth2 Bearer (Telegram, Gmail, ...): CAN be host-side (A1).** The proxy
  already injects a header per request, so the secret can stay out of the container. This
  is the existing, preferred model. No real tension.
- **DPoP (Bluesky): the ONLY genuine judgment call.** It is request-level (so the host
  COULD sign), but signing needs the private key per request, so host-side means real
  per-request signing logic in the proxy path:
  - **A1 (host signs):** the DPoP key stays host-side, never in the container; cost is
    per-request DPoP signing at the proxy, real provider logic in the proxy, not "inject a
    string".
  - **A2 (plugin signs):** goclawkit ships the signer (literally `bskyoauth`'s
    RoundTripper); the host just delivers the credential blob; cost is the DPoP key lives
    in the container (weaker), re-coupling to "secret in the sandbox".

So fork A is not one global decision; it is per-scheme, and only DPoP is actually open.
The shape goclaw must support is therefore HYBRID by necessity: host-side auth for the
request-level schemes that allow it, in-plugin auth for the connection/session schemes
that require it. The only thing to decide is which side DPoP lands on (section 9).

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

The survey (1.5) resolved most of fork A: goclaw must support HYBRID auth (host-side for
request-level schemes that allow it, in-plugin for connection/session schemes that require
it). What actually remains open:

1. **DPoP's side (the only real fork-A question):** A1 host-signs (DPoP key out of the
   container, per-request signing in the proxy) vs A2 plugin-signs (`bskyoauth`'s
   RoundTripper in the plugin, key in the container). Not needed until we build Bluesky;
   the schema (section 3) just must be able to hold the DPoP bundle either way.
2. **Fork B (delivery) for the host-side schemes:** for Bearer, proxy-inject vs a
   credential-to-plugin file. This is the live "proxy is opt-in" tension and the one that
   affects Gmail NOW.
3. **Gmail right now:** low-risk default is host-side Authenticator + proxy-inject for the
   Bearer; the open part is only fork B (whether to also support a file path so OAuth works
   with the proxy off). DPoP (1) is deferred; connection/session schemes are settled
   (in-plugin by necessity).

## 10. What was built (OAuth2-Bearer / Gmail)

Decisions taken (for the Bearer case only; DPoP and connection/session schemes unchanged):

- **Fork A = host-side.** The authenticator lives on the host (credstore), not in the
  plugin. The refresh token and minted access tokens never enter the container.
- **Fork B = proxy-inject ONLY.** The credential reaches the request through the
  TLS-intercepting proxy, which sets `Authorization: Bearer <access_token>` for the target
  host, exactly as it already does for a static GitHub/Anthropic token. No token-file path
  was built; Gmail therefore requires the credproxy to be ON (the opt-in cost is accepted,
  in exchange for the secret never entering the container). A file path can be added later
  if "OAuth with the proxy off" is ever wanted.

The pieces, and where they live:

- **credstore engine** (`internal/credstore/oauth.go`): `kind=oauth2-bearer` rows store an
  opaque, encrypted `oauth2Bundle` (refresh token, client id/secret, token_url, scopes,
  cached access token + expiry). `AccessToken(ctx, host)` returns a current access token,
  refreshing within ~60s of expiry; refresh is single-flight per host (no stampede, no
  burning a rotating refresh token N times); a rotated refresh token is persisted back.
- **Kind-dispatching resolve** (`BearerForHost`, `UpstreamForHost`): the proxy asks
  `UpstreamForHost` at CONNECT (cheap existence check, no mint) to decide
  intercept-vs-blind-tunnel, then `BearerForHost` PER REQUEST in the Director, so a
  keep-alive tunnel always injects a CURRENT token (an access token can expire mid-tunnel).
  `BearerForHost` dispatches on kind: static -> stored token, oauth2-bearer -> a refreshed
  access token. The proxy never knows about kinds.
- **Refresh path stays host-side and direct.** credstore's refresh HTTP client talks to the
  token endpoint directly (system roots), NOT back through the proxy, so there is no
  recursion and the token endpoint needs no stored credential of its own.
- **Consent** (`goclaw auth add-oauth`, `cmd/goclaw/auth_oauth.go`): three paths, in order
  of preference: `--refresh-token <rt>` (store directly, no browser); default loopback
  (open the browser to Google's consent screen with `access_type=offline&prompt=consent`,
  catch the `?code=` on an ephemeral `127.0.0.1` server that lives ONLY for the command,
  exchange it); `--no-browser` (print the URL, paste the code, no server). Google-only for
  now (`--provider google`).

Not built (still as designed above): DPoP/atproto (`AccessToken` and the bundle schema can
hold its key+nonce, but no signer); a token-file delivery path; connection/session schemes.
