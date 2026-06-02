// Package telegram implements the ChannelAdapter for Telegram using long-poll
// mode — no inbound port or public URL needed, which fits a self-hosted box
// behind NAT (brief §7.5). Telegram is the v0 channel; more channels drop in
// behind the same interface later (brief §7.4).
package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/time/rate"

	"github.com/shindakun/goclaw/internal/channels"
)

// channelName is the stable id used across the host and in the DB.
const channelName = "telegram"

// Adapter is the Telegram ChannelAdapter.
type Adapter struct {
	bot     *tgbotapi.BotAPI
	limiter *rate.Limiter // outbound rate limit (Telegram ~30 msg/s, brief §7.3)
}

// New constructs a Telegram adapter from a bot token (load from env/vault —
// never mount it into the agent container, brief §7.6).
func New(token string) (*Adapter, error) {
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("telegram: connect: %w", err)
	}
	return &Adapter{
		bot: bot,
		// Stay comfortably under Telegram's ~30 msg/s ceiling.
		limiter: rate.NewLimiter(rate.Limit(25), 1),
	}, nil
}

// Name implements ChannelAdapter.
func (a *Adapter) Name() string { return channelName }

// Start begins long-polling and streams normalized inbound messages until ctx
// is cancelled. The tgbotapi update channel is shut down via StopReceivingUpdates.
func (a *Adapter) Start(ctx context.Context) (<-chan channels.InboundMsg, error) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30 // long-poll seconds
	updates := a.bot.GetUpdatesChan(u)

	out := make(chan channels.InboundMsg)
	go func() {
		defer close(out)
		defer a.bot.StopReceivingUpdates()
		for {
			select {
			case <-ctx.Done():
				return
			case upd, ok := <-updates:
				if !ok {
					return
				}
				msg := upd.Message
				if msg == nil {
					continue // ignore edits, callbacks, etc. for v0
				}
				raw, _ := json.Marshal(upd)
				in := channels.InboundMsg{
					Channel:   channelName,
					ChatID:    strconv.FormatInt(msg.Chat.ID, 10),
					SenderID:  senderID(msg),
					Sender:    senderName(msg),
					Text:      msg.Text,
					Raw:       raw,
					Timestamp: time.Unix(int64(msg.Date), 0),
				}
				// TODO: map msg.Document / msg.Photo into channels.Attachment
				// and resolve file URLs via a.bot.GetFile for the ingest path.
				select {
				case out <- in:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// Send delivers a reply, respecting the outbound rate limit.
func (a *Adapter) Send(ctx context.Context, m channels.OutboundMsg) error {
	if err := a.limiter.Wait(ctx); err != nil {
		return err
	}
	chatID, err := strconv.ParseInt(m.ChatID, 10, 64)
	if err != nil {
		return fmt.Errorf("telegram: bad chat id %q: %w", m.ChatID, err)
	}
	if _, err := a.bot.Send(tgbotapi.NewMessage(chatID, m.Text)); err != nil {
		return fmt.Errorf("telegram: send: %w", err)
	}
	// TODO: send m.Attachments and honor m.Action (typing indicator, etc.).
	return nil
}

func senderID(m *tgbotapi.Message) string {
	if m.From == nil {
		return ""
	}
	return strconv.FormatInt(m.From.ID, 10)
}

func senderName(m *tgbotapi.Message) string {
	if m.From == nil {
		return ""
	}
	if m.From.UserName != "" {
		return "@" + m.From.UserName
	}
	return m.From.FirstName
}
