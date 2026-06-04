# Channels: setup and config tips

goclaw talks to messaging channels through one `ChannelAdapter` interface, so
each channel is enabled independently by setting its token. This is the practical
setup guide, including the gotchas that are easy to hit.

## Telegram

1. Message [@BotFather](https://t.me/BotFather) → `/newbot`, follow the prompts,
   copy the bot token.
2. Set in `.env`:
   ```sh
   TELEGRAM_BOT_TOKEN=...            # from @BotFather
   GOCLAW_OWNER_TELEGRAM_ID=...      # your numeric user id (see below)
   ```
3. Restart the host and message the bot.

**Finding your Telegram user id:** message the bot once, then read the host log;
the line shows `sender=<your id>`. (Or use a bot like @userinfobot.)

## Discord

### Create the bot

1. [Discord Developer Portal](https://discord.com/developers/applications) →
   **New Application**.
2. Left sidebar → **Bot** → copy the **token** (this is `GOCLAW_DISCORD_TOKEN`;
   no `Bot ` prefix). Keep **Public Bot** OFF (it can stay private and you can
   still invite it to your own server).
3. On the same **Bot** page, under **Privileged Gateway Intents**, enable
   **Message Content Intent**. Without this the bot connects but every message
   arrives with empty text, so the agent sees nothing.

### Invite it to your server

Discord's newer install system can make a hand-built `?scope=bot` invite URL fail
with **"installation type not supported."** Two ways around it:

- **Easiest:** Developer Portal → your app → **Installation**. Ensure **Guild
  Install** is checked, set **Install Link → Discord Provided Link**, and under
  the Guild Install default settings add the `bot` scope plus the permissions
  (View Channels, Send Messages). Copy the **Discord-provided link** at the top
  and open it, then pick your server and Authorize.
- **Manual:** on the **Installation** page set **Install Link → None**, then use a
  hand-built URL (replace `CLIENT_ID` with your Application ID from **General
  Information**):
  ```
  https://discord.com/api/oauth2/authorize?client_id=CLIENT_ID&scope=bot&permissions=3072
  ```
  `permissions=3072` = View Channels + Send Messages. You do NOT need a redirect
  URL or a public bot for this; those are for OAuth web logins, not bot invites.

After authorizing, if your target channel is **private**, the server-level invite
may not be enough: open that channel's settings → Permissions → add the bot (or
its role) → allow View Channel + Send Messages.

### Configure

```sh
# .env
GOCLAW_DISCORD_TOKEN=...        # the bot token
GOCLAW_OWNER_DISCORD_ID=...     # your Discord USER id (see below)
```

**Finding your Discord user id:** Settings (gear, bottom-left) → **Advanced** →
toggle **Developer Mode** on, then close settings. Now copy your id:

- **Web / desktop app:** click your avatar/name at the bottom-left to open your
  profile popout, click the **three dots (⋯)**, then **Copy User ID**. (Right-
  clicking your name also works in the desktop app, but the three-dots popout is
  the reliable path in the browser.)

This is YOUR id, not the bot's client/application id. The two numbers in a channel
URL `discord.com/channels/<server>/<channel>` are the server id and channel id,
neither of which is your user id.

## Owner identity and the access gate

`GOCLAW_OWNER_*_ID` seeds you as the "owner" so your messages pass the access gate
(unknown senders are denied by default). The same owner user can hold both a
Telegram and a Discord identity, so you can reach the agent from either channel.

If you skip the owner id, an unknown sender's first message is held and you (an
existing owner) must approve it.

## Scheduled vault maintenance target

When a vault is configured, the scheduled maintenance jobs post their one-line
summary to the owner: the Telegram owner if `GOCLAW_OWNER_TELEGRAM_ID` is set,
otherwise the Discord owner's DM (the bot opens a DM to the owner, since Discord
posts to channels, not user ids). Maintenance is skipped if no owner channel is
configured.
