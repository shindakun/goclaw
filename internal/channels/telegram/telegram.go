// Package telegram implements the ChannelAdapter for Telegram using long-poll
// mode - no inbound port or public URL needed, which fits a self-hosted box
// behind NAT (brief §7.5). Telegram is the v0 channel; more channels drop in
// behind the same interface later (brief §7.4).
package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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

// New constructs a Telegram adapter from a bot token (load from env/vault -
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
				// Telegram puts a caption (not Text) on media messages; prefer
				// whichever is present so the agent sees the user's words.
				body := msg.Text
				if body == "" {
					body = msg.Caption
				}
				body, atts := mapAttachments(body, msg)
				in := channels.InboundMsg{
					Channel:     channelName,
					ChatID:      strconv.FormatInt(msg.Chat.ID, 10),
					SenderID:    senderID(msg),
					Sender:      senderName(msg),
					Text:        body,
					Attachments: atts,
					Raw:         raw,
					Timestamp:   time.Unix(int64(msg.Date), 0),
				}
				// TODO: resolve file URLs via a.bot.GetFile for the ingest path so
				// the agent can fetch the bytes, not just see the placeholder.
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

// telegramMaxMessageLen is Telegram's per-message character limit; a longer reply
// is rejected, so we split it across messages.
const telegramMaxMessageLen = 4096

// Send delivers a reply, respecting the outbound rate limit. Replies over
// Telegram's 4096-character limit are split into several messages (each chunk
// rate-limited in turn) so a long agent answer is not dropped.
func (a *Adapter) Send(ctx context.Context, m channels.OutboundMsg) error {
	chatID, err := strconv.ParseInt(m.ChatID, 10, 64)
	if err != nil {
		return fmt.Errorf("telegram: bad chat id %q: %w", m.ChatID, err)
	}
	for _, chunk := range channels.SplitMessage(m.Text, telegramMaxMessageLen) {
		if err := a.limiter.Wait(ctx); err != nil {
			return err
		}
		if _, err := a.bot.Send(tgbotapi.NewMessage(chatID, chunk)); err != nil {
			return fmt.Errorf("telegram: send: %w", err)
		}
	}
	// TODO: send m.Attachments.
	return nil
}

// SendAction implements channels.ActionSender, mapping a normalized action kind
// to a Telegram chat action (sendChatAction). The indicator auto-expires after
// ~5s, so callers that want it to persist must re-send periodically.
func (a *Adapter) SendAction(ctx context.Context, chatID, kind string) error {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("telegram: bad chat id %q: %w", chatID, err)
	}
	action := telegramAction(kind)
	if action == "" {
		return nil // unknown kind - no-op rather than error
	}
	if _, err := a.bot.Request(tgbotapi.NewChatAction(id, action)); err != nil {
		return fmt.Errorf("telegram: chat action: %w", err)
	}
	return nil
}

// telegramAction maps a normalized action kind to a Telegram action string.
func telegramAction(kind string) string {
	switch kind {
	case "typing":
		return tgbotapi.ChatTyping
	default:
		return ""
	}
}

// mapAttachments inspects a Telegram message for media and, when present, appends
// a typed placeholder line to text (so the agent, which reads only Text, knows
// something non-text arrived) and returns matching channels.Attachment values.
// Telegram media URLs are not in the update: fetching bytes needs an async
// GetFile call (see the TODO at the call site), so URL is left empty here and the
// FileID is carried so a later ingest step can resolve it.
func mapAttachments(text string, m *tgbotapi.Message) (string, []channels.Attachment) {
	var (
		placeholders []string
		atts         []channels.Attachment
	)
	add := func(placeholder, fileID, filename, mime string) {
		placeholders = append(placeholders, placeholder)
		atts = append(atts, channels.Attachment{
			Filename: filename,
			MIMEType: mime,
			URL:      fileID, // a Telegram file_id, resolved to a URL later
		})
	}

	switch {
	case len(m.Photo) > 0:
		// Photo is a slice of sizes; the last is the largest. No filename.
		p := m.Photo[len(m.Photo)-1]
		add("[Image]", p.FileID, "", "image/jpeg")
	case m.Document != nil:
		name := m.Document.FileName
		if name == "" {
			name = "file"
		}
		add("[File: "+name+"]", m.Document.FileID, m.Document.FileName, m.Document.MimeType)
	case m.Video != nil:
		add("[Video]", m.Video.FileID, m.Video.FileName, m.Video.MimeType)
	case m.Audio != nil:
		name := m.Audio.FileName
		if name == "" {
			name = "audio"
		}
		add("[Audio: "+name+"]", m.Audio.FileID, m.Audio.FileName, m.Audio.MimeType)
	case m.Voice != nil:
		add("[Voice]", m.Voice.FileID, "", m.Voice.MimeType)
	case m.Sticker != nil:
		add("[Sticker]", m.Sticker.FileID, "", "")
	}

	if len(placeholders) == 0 {
		return text, nil
	}
	joined := strings.Join(placeholders, "\n")
	if text == "" {
		return joined, atts
	}
	return text + "\n" + joined, atts
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
