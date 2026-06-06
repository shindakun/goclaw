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

## 5. Build order

1. `kind: channel` gmail plugin in goclawkit: `Start` poll loop (query, emit inbound,
   mark-read for dedup), `Send` (threaded reply). Auth via 3a (token file) to unblock.
   Worked example + tests like the IRC plugin.
2. Prove it end to end through the existing boundary (it is a 4a dialer, so this is the
   IRC path with a poll loop). Eager-launch + pin already apply.
3. Credential-proxy OAuth refresh (`docs/credproxy-oauth.md`): hold the refresh token in
   credstore, mint access tokens, inject per request. Swap the plugin's auth from 3a to
   3b (placeholder in the container, real token injected by the proxy).
4. Optional: the companion `kind: tool` gmail-tools plugin, reusing the same auth.

## 6. Open questions

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
