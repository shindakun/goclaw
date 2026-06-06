# Gmail channel plugin: design

Status: DESIGN. This maps a Gmail integration onto goclaw's channel-plugin model and
identifies the one genuinely new thing it forces: OAuth token lifecycle in the
credential proxy. Read `docs/channels-plugin-design.md` (the channel-plugin boundary)
and `docs/security.md` (the credential proxy) first.

The reference is NanoClaw's `add-gmail` skill, a poll-based channel: it queries the
Gmail API for unread Primary mail every ~minute, an unread email triggers the agent,
and the reply is sent as an email. It is ALSO offered as a tool (read/send/search/draft
on demand). We are not copying its mechanism (it merges TypeScript into a source tree
and mounts the live OAuth token into the container); we are taking the SHAPE and fitting
it to goclaw.

## 1. The 80% that is already built

Gmail, as a channel, is an OUTBOUND-DIALER channel (section 4a in the channel-plugin
design): it dials OUT to `gmail.googleapis.com` and polls. It has no inbound listener
and no open port. That means it slots onto everything we already built for IRC with no
new boundary work:

- It is a `kind: channel` plugin, launched IN the container by the runner, bridged to
  the host relay over the existing socket boundary (TCP on macOS, Unix on Linux).
- It hot-adds, and the eager-launch + sweep-pin (an always-on channel keeps its
  container up) apply unchanged.
- It implements the same `ServeChannel` contract IRC does. The plugin author writes
  `Start`/`Send`; the SDK and the host handle the protocol.

The manifest:

```yaml
name: gmail
kind: channel
version: "1.0.0"
exec: gmail
description: Gmail poll channel; unread Primary email triggers the agent, replies send as email.
env:
  - GMAIL_POLL_INTERVAL   # default 60s
  - GMAIL_QUERY           # default "is:unread category:primary"
  # auth: see section 3 (the real design question)
```

`Start()` is a poll loop, not a held connection:

1. Every `GMAIL_POLL_INTERVAL`, query Gmail for messages matching `GMAIL_QUERY`.
2. For each new message: emit a `channel.inbound` with
   - `ChatID` = the Gmail THREAD id, so the agent's reply threads correctly;
   - `SenderID` = the From address (the host namespaces it to `gmail:<addr>` at the
     relay, per the section-7 identity rule, never trusted as an owner id);
   - `Sender` = the From display name;
   - `Text` = subject + a plain-text rendering of the body.
3. Mark the message read (or label it) so the next poll does not re-fire it. This is the
   dedup story; IRC had none because each PRIVMSG is seen once. Gmail MUST do this or
   every poll resurfaces the same unread mail.

`Send(out)` sends the agent's reply as an email in `out.ChatID`'s thread (Gmail
`messages.send` with `In-Reply-To` / `References` set from the thread), so a reply lands
as a normal email reply, not a new thread.

So the channel half is "IRC with a poll loop and a different upstream." No changes to
the boundary, the relay, the env allowlist, the eager-launch, or the sweep-pin.

## 2. Differences from IRC, summarized

| Concern | IRC | Gmail |
|---|---|---|
| Transport | hold a TLS socket | poll an HTTPS API |
| Channel shape | 4a dialer | 4a dialer (poll variant) |
| Auth | none (spoofable nick) | OAuth2 (token expires, needs refresh) |
| Secret handling | n/a | the real design question (section 3) |
| ChatID | channel / nick | email thread id |
| Dedup | n/a (each line seen once) | mark-read so a poll does not re-fire |
| Tool side | none | optional companion `kind: tool` plugin (section 4) |

Everything except auth and dedup is mechanical. Auth is the part with weight.

## 3. The real design question: where does the OAuth token live?

IRC needed no credential. Gmail needs an OAuth2 token, and that token is exactly the
kind of secret goclaw's whole credential model exists to keep out of the untrusted
container. NanoClaw mounts the live `credentials.json` into the container; for goclaw
that is the thing we explicitly avoid (a prompt-injected agent could read it). Two
options, mirroring `docs/security.md`'s two credential layers:

### 3a. Simple: token in the plugin (matches NanoClaw, accept the posture)

The OAuth access token is a plugin credential, delivered via the env allowlist (e.g. a
token file mounted into the plugin dir, named by `GMAIL_TOKEN_PATH`). The untrusted
plugin holds the token and refreshes it itself.

- PRO: no new host machinery; the plugin is self-contained.
- CON: the live token sits in the sandbox, readable by a hostile/injected plugin or
  agent. This is the same posture as the direct-env-key fallback documented in
  security.md, defensible for a personal bot, but it is the weaker choice and it puts
  refresh logic (and the refresh token, the MORE sensitive secret) inside the box too.

### 3b. Proper: route Gmail through the credential proxy (goclaw-native)

The container holds a placeholder; the proxy injects the real `Authorization: Bearer
<access-token>` per request to `gmail.googleapis.com`, exactly as it does for Anthropic
and GitHub today. The plugin never sees the token. The HTTPS to Gmail is intercepted the
same two-session-splice way the proxy already does (see security.md "How interception
works").

The load-bearing simplification for the PLUGIN AUTHOR: in this mode the plugin sends NO
auth header at all. It makes a plain `GET https://gmail.googleapis.com/...` with no
`Authorization`, and the proxy adds the bearer on the way out. The plugin does not hold,
refresh, or even know the token. All it must do is make its HTTP requests through a
client that honors the container's `HTTPS_PROXY` and trusts the proxy CA, which is the
container default (see section 5a). This is exactly why we push OAuth host-side: the
plugin stays a dumb Gmail-API client, and every OAuth concern (refresh, rotation,
single-flight, the refresh token itself) lives in goclaw, reusable by the next OAuth
channel.

This is strictly better and reuses machinery we have, EXCEPT for one thing the proxy
does not do yet: **OAuth access tokens expire (~1h) and must be refreshed.** Today the
proxy injects a STATIC token (`injectAuth(req, host, token)`, a fixed string). Gmail
forces the proxy to hold a REFRESH token (host-side, in `credstore`) and mint short
-lived access tokens on demand, refreshing when they expire.

That is the architectural delta, and it is worth doing because it GENERALIZES: every
OAuth upstream (Slack, Google Calendar/Drive, any "sign in with Google" API) needs the
same thing. Gmail is just the first case that forces it. The extension is specified
separately in `docs/credproxy-oauth.md` (next), because it has real weight of its own.

### Recommendation

Build 3b (proxy-injected OAuth) as the target, because it keeps the secret out of the
sandbox and the refresh machinery generalizes. 3a is an acceptable bring-up shortcut to
prove the channel/poll/dedup/threading mechanics WITHOUT blocking on the proxy work,
then swap auth to 3b. The channel code is identical either way (the plugin just makes
HTTPS calls to Gmail); only WHERE the token is injected differs, which is the whole
point of the proxy design.

## 4. The channel + tool duality

NanoClaw's Gmail is both a channel (email triggers the agent) and a tool (the agent
reads/sends/searches/drafts on demand). goclaw's plugin model supports both KINDS, but a
single plugin is one kind today. Two clean ways:

- **Two plugins, one repo (recommended start):** a `kind: channel` `gmail` (the poll
  loop above) and a `kind: tool` `gmail-tools` (read/send/search/draft), sharing the same
  auth path. Zero changes to the plugin model; matches how the system already works.
- **One plugin advertising both:** extend the manifest so a plugin can be a channel AND
  expose tools. More work, defer unless the two-plugin split proves annoying in use.

Start with two plugins. They share the auth story (section 3) and the upstream
(`gmail.googleapis.com`), so the proxy work serves both.

## 5. The goclawkit side: what the plugin author actually writes (and what the SDK owes them)

The sections above are host-first. From the plugin-author (goclawkit) view, a Gmail poll
channel needs things the channel SDK does NOT provide yet. The IRC plugin got away
without them because it holds a socket and has no auth; a poll-and-OAuth channel exposes
the gaps. Three buckets: what the author writes, what the SDK should provide so every
poll/HTTP channel does not reinvent it, and the auth contract.

### 5a. The HTTP-client contract (the most important, least obvious thing)

Under 3b (proxy-injected OAuth, the target), the plugin sends NO auth header: it makes
plain HTTPS calls to `gmail.googleapis.com` and the proxy adds the bearer. For that to
work the plugin's `http.Client` MUST:

- honor the container's `HTTPS_PROXY` env (Go's `http.DefaultTransport` does this via
  `ProxyFromEnvironment`, so a default client already works, but a plugin that builds a
  custom `Transport` and forgets `Proxy: http.ProxyFromEnvironment` will bypass the proxy
  and the request will go out with NO auth and fail);
- trust the proxy CA. The container sets `SSL_CERT_FILE` / `NODE_EXTRA_CA_CERTS` to the
  mounted CA, and Go's TLS honors `SSL_CERT_FILE` for the system pool, so again a default
  client works, but a plugin that sets its own `tls.Config{RootCAs: ...}` will reject the
  proxy's leaf.

So the SDK should give authors a `plugin.HTTPClient()` (or document the rule loudly): use
the default-derived, proxy-and-CA-aware client; do NOT hand-roll a Transport unless you
replicate `ProxyFromEnvironment` and the system cert pool. This is a footgun worth a
helper, because "my requests bypass the proxy and fail with 401" is an opaque way to
learn it. Under 3a (token in plugin) the same client is used; the plugin just also sets
its own `Authorization` header. The HTTP CLIENT is identical between modes; only whether
the plugin adds a header differs (see 5c).

### 5b. Poll-channel scaffolding the SDK should provide

IRC's `Start` is a bespoke read loop. A poll channel's `Start` is a different but equally
generic shape that every poll-based channel (Gmail, an RSS feed, a status API) repeats:
tick on an interval, fetch, dedup, emit. The SDK should offer a `ServePoll`-style helper
so authors implement only the fetch, not the loop:

```go
// sketch, goclawkit
type Poller interface {
    Info() Info
    // Poll is called on each tick; it returns the new inbound messages since the last
    // call. The SDK handles the ticker, ctx cancellation, backoff on error, and feeding
    // the returned Inbounds up the channel.* protocol.
    Poll(ctx context.Context) ([]Inbound, error)
    // Send delivers a reply (same as Channel.Send).
    Send(ctx context.Context, out Outbound) error
}
```

What the SDK owns in `ServePoll`: the ticker (interval from env/Info), ctx-cancel on
shutdown, error backoff (do not hammer on a 500/quota error), and bridging to
`ServeChannel` underneath (it IS a channel, just with a built-in loop). What the AUTHOR
owns: one `Poll` that queries Gmail and returns new `Inbound`s, and `Send`. This turns
the Gmail plugin's interesting code into "map a Gmail message to an Inbound" and "map an
Outbound to a Gmail send", which is the actual domain work.

DEDUP is the subtlety the SDK should help with but cannot fully own. "New since last
call" requires state: Gmail does it by mutating the inbox (mark-read) so the next query
excludes seen mail, which is domain-specific and the author's job. But a channel that
CANNOT mutate the source (an RSS feed) needs a "seen ids" set the SDK could offer as an
optional helper (a bounded set persisted in the plugin dir). For Gmail specifically,
mark-read is the dedup and the SDK helper is not needed; the doc notes it so the
`ServePoll` design does not pretend dedup is free.

### 5c. The auth contract, from the plugin's side

The plugin should NOT branch on "am I in mode 3a or 3b." That is a deployment choice, not
plugin logic. The clean contract:

- The plugin ALWAYS makes Gmail requests through the proxy-aware client (5a).
- In 3b, it adds no `Authorization`; the proxy injects it. In 3a, it reads a token from
  its allowlisted env/file and sets the header itself.
- To keep the plugin from branching, define ONE behavior: the plugin sets
  `Authorization` from an env var (e.g. `GMAIL_BEARER`) IF AND ONLY IF that var is set,
  and otherwise sends none. In 3b the host leaves it unset (the proxy injects); in 3a the
  host sets it to the token. The plugin code is identical; the deployment decides.

This is the goclawkit-friendly framing of "OAuth lives host-side": the plugin's entire
auth surface is "set a bearer from env if present." Everything hard (refresh, rotation,
the refresh token) is in goclaw, and the plugin cannot leak what it never holds.

### 5d. What goclawkit must add, concretely

- `plugin.HTTPClient()`: the proxy-and-CA-aware default client (or a documented rule +
  lint note). Small, prevents the most likely footgun.
- `ServePoll(Poller)`: the poll-loop runtime (ticker, backoff, ctx, bridge to
  ServeChannel). Generic; serves Gmail and any future poll channel.
- A worked `cmd/gmail` example exercising both, like `cmd/irc` and `cmd/webhook` do for
  their shapes, with a `-selftest` that runs the poll/dedup/Send round trip against an
  in-process fake Gmail API (no real network, no OAuth), the same hermetic-demo discipline
  the IRC and webhook examples follow.
- NO OAuth code in goclawkit. The refresh machinery is goclaw-side
  (`docs/credproxy-oauth.md`); the kit stays a thin client. This is the whole point of
  your steer: keep the kit dumb, keep OAuth generic and host-side.

## 6. Build order

0. goclawkit SDK additions (section 5d): `plugin.HTTPClient()` and `ServePoll(Poller)`,
   so the Gmail plugin (and every future poll channel) implements only `Poll` + `Send`.
   Small, generic, no OAuth code.
1. `kind: channel` gmail plugin in goclawkit on top of `ServePoll`: `Poll` (query, map to
   inbound, mark-read for dedup), `Send` (threaded reply). Auth via the env-bearer
   contract (5c); set `GMAIL_BEARER` from a token file for 3a bring-up. Worked example
   with a `-selftest` against a fake Gmail API, like the IRC plugin.
2. Prove it end to end through the existing boundary (it is a 4a dialer, so this is the
   IRC path with a poll loop). Eager-launch + pin already apply.
3. Credential-proxy OAuth refresh (`docs/credproxy-oauth.md`): hold the refresh token in
   credstore, mint access tokens, inject per request. The swap from 3a to 3b is now a
   DEPLOYMENT change, not a code change: the host stops setting `GMAIL_BEARER` (so the
   plugin sends no header) and the proxy injects the bearer instead. The plugin binary is
   untouched, the payoff of the 5c env-bearer contract.
4. Optional: the companion `kind: tool` gmail-tools plugin, reusing the same auth.

## 7. Open questions

- Body rendering: Gmail messages are MIME multipart (HTML + text + attachments). The
  plugin must extract a sane plain-text body for the agent and decide what to do with
  attachments (drop, summarize, or feed to the ingest path). Start: plain-text part
  only, note attachments by filename.
- Filtering: NanoClaw defaults to `is:unread category:primary` and lets the user narrow
  by sender/label/keyword with no code change (it is just the query string). Same here:
  `GMAIL_QUERY` is the knob.
- Rate / quota: polling every 60s is well within Gmail API quota for one user, but the
  plugin should back off on quota errors rather than hammer.
- Mark-read vs label: marking read is simplest for dedup but mutates the user's inbox
  (the mail no longer shows unread). A dedicated label (`goclaw-seen`) is less invasive
  but more setup. Default to mark-read, make it configurable.
