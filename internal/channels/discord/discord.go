// Package discord implements the ChannelAdapter for Discord using the gateway
// websocket (discordgo) - no inbound port or public URL needed, which fits a
// self-hosted box behind NAT (brief §7.5). It mirrors the Telegram adapter:
// text in/out plus a typing indicator; identity is normalized to Discord's
// stable numeric user id. Attachments are not mapped yet (parity with Telegram).
package discord

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bwmarrin/discordgo"
	"golang.org/x/time/rate"

	"github.com/shindakun/goclaw/internal/channels"
)

// channelName is the stable id used across the host and in the DB.
const channelName = "discord"

// Adapter is the Discord ChannelAdapter.
type Adapter struct {
	session *discordgo.Session
	selfID  string        // the bot's own user id, to ignore its own messages
	limiter *rate.Limiter // light outbound rate limit (discordgo also handles 429s)
}

// New constructs a Discord adapter from a bot token (load from env/vault - never
// mount it into the agent container, brief §7.6). The token must NOT include the
// "Bot " prefix; discordgo adds it.
func New(token string) (*Adapter, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("discord: session: %w", err)
	}
	// We need message content to read text (Discord requires the privileged
	// Message Content Intent, which must also be enabled in the Developer Portal).
	s.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentMessageContent
	return &Adapter{
		session: s,
		limiter: rate.NewLimiter(rate.Limit(20), 1),
	}, nil
}

// Name implements ChannelAdapter.
func (a *Adapter) Name() string { return channelName }

// Start opens the gateway and streams normalized inbound messages until ctx is
// cancelled. discordgo owns reconnect/backoff internally.
func (a *Adapter) Start(ctx context.Context) (<-chan channels.InboundMsg, error) {
	out := make(chan channels.InboundMsg)

	a.session.AddHandler(func(_ *discordgo.Session, m *discordgo.MessageCreate) {
		// Ignore the bot's own messages (else it would reply to itself).
		if a.selfID != "" && m.Author != nil && m.Author.ID == a.selfID {
			return
		}
		if m.Author != nil && m.Author.Bot {
			return // ignore other bots too
		}
		raw, _ := json.Marshal(m)
		in := channels.InboundMsg{
			Channel:   channelName,
			ChatID:    m.ChannelID,
			SenderID:  senderID(m),
			Sender:    senderName(m),
			Text:      m.Content,
			Raw:       raw,
			Timestamp: m.Timestamp,
		}
		// Deliver, but never block past ctx cancellation.
		select {
		case out <- in:
		case <-ctx.Done():
		}
	})

	if err := a.session.Open(); err != nil {
		return nil, fmt.Errorf("discord: open gateway: %w", err)
	}
	if a.session.State != nil && a.session.State.User != nil {
		a.selfID = a.session.State.User.ID
	}

	// Close the gateway when ctx is cancelled.
	go func() {
		<-ctx.Done()
		a.session.Close()
		close(out)
	}()
	return out, nil
}

// DMChannelID resolves a Discord user id to a DM channel id (opening the DM if
// needed), so callers that only know a user (e.g. the maintenance scheduler
// targeting the owner) can post to them. Discord cannot post to a user id
// directly; messages go to a channel. The gateway must be open first.
func (a *Adapter) DMChannelID(userID string) (string, error) {
	ch, err := a.session.UserChannelCreate(userID)
	if err != nil {
		return "", fmt.Errorf("discord: open DM to %s: %w", userID, err)
	}
	return ch.ID, nil
}

// Send delivers a reply, respecting the outbound rate limit.
func (a *Adapter) Send(ctx context.Context, m channels.OutboundMsg) error {
	if err := a.limiter.Wait(ctx); err != nil {
		return err
	}
	if _, err := a.session.ChannelMessageSend(m.ChatID, m.Text); err != nil {
		return fmt.Errorf("discord: send: %w", err)
	}
	// TODO: send m.Attachments.
	return nil
}

// SendAction implements channels.ActionSender, mapping a normalized action kind
// to a Discord action. Discord's typing indicator auto-expires after ~10s, so a
// caller that wants it to persist must re-send periodically.
func (a *Adapter) SendAction(ctx context.Context, chatID, kind string) error {
	if kind != "typing" {
		return nil // unknown kind - no-op rather than error
	}
	if err := a.session.ChannelTyping(chatID); err != nil {
		return fmt.Errorf("discord: typing: %w", err)
	}
	return nil
}

func senderID(m *discordgo.MessageCreate) string {
	if m.Author == nil {
		return ""
	}
	return m.Author.ID
}

func senderName(m *discordgo.MessageCreate) string {
	if m.Author == nil {
		return ""
	}
	if m.Author.Username != "" {
		return "@" + m.Author.Username
	}
	return m.Author.ID
}
