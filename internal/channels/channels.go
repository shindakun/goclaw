// Package channels defines the host-side communication layer: the
// ChannelAdapter interface every messaging channel implements, the normalized
// message types that cross the SQLite boundary, and a registry the host uses to
// dispatch outbound replies (brief §7).
//
// Adapters do ONLY transport + identity normalization. Routing and permission
// decisions belong to internal/router and internal/permissions; delivery
// authorization belongs to internal/delivery. This separation is what lets a
// new channel be added without touching security code.
package channels

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ChannelAdapter is implemented once per channel (Telegram, Slack, ...).
// The host treats every channel uniformly through this interface.
type ChannelAdapter interface {
	// Name returns the channel id ("telegram", "slack", "discord", ...).
	Name() string

	// Start connects (long-poll, websocket, or webhook) and streams normalized
	// inbound messages on the returned channel until ctx is cancelled. The
	// implementation owns reconnect/backoff; it should not return an error for
	// transient drops, only for unrecoverable setup failures.
	Start(ctx context.Context) (<-chan InboundMsg, error)

	// Send delivers one reply. Called only by the delivery goroutine, and only
	// after the delivery-authorization check has passed (brief §7.6, §9).
	Send(ctx context.Context, out OutboundMsg) error
}

// ActionSender is an OPTIONAL capability: a channel that can show a transient
// chat action (e.g. Telegram's "typing…"). The typing manager checks for it
// with a type assertion, so channels that don't implement it simply show no
// indicator - no change to the core ChannelAdapter contract.
type ActionSender interface {
	// SendAction shows a transient action in a chat. kind is a normalized name
	// ("typing", …); the adapter maps it to its channel-native action.
	SendAction(ctx context.Context, chatID, kind string) error
}

// InboundMsg is a channel message normalized for the router. Everything here is
// attacker-controllable and enters the trust-tiering regime (brief §11.6).
type InboundMsg struct {
	Channel     string          // "telegram"
	ChatID      string          // channel-native conversation id
	SenderID    string          // channel-native, STABLE user id
	Sender      string          // display name (best-effort)
	Text        string          // message body
	Attachments []Attachment    // files/images → feed §11 ingest
	Raw         json.RawMessage // original payload, for debugging
	Timestamp   time.Time
}

// OutboundMsg is a reply the delivery loop hands to an adapter's Send.
type OutboundMsg struct {
	Channel     string
	ChatID      string
	Text        string
	Attachments []Attachment
	Action      *SystemAction // optional: typing indicator, reaction, etc.
}

// Attachment is a file or image carried by a message.
type Attachment struct {
	Filename string
	MIMEType string
	URL      string // channel-hosted URL, or a local path once downloaded
	Bytes    []byte // populated only when inlined
}

// SystemAction is an optional non-text side effect on outbound delivery.
type SystemAction struct {
	Kind string // "typing", "reaction", ...
	Data string
}

// maxAttachmentLabelRunes caps a sanitized attachment label. A filename is
// attacker-controlled and is rendered into the message text the agent reads, so an
// unbounded one could flood the prompt; a real filename is well under this.
const maxAttachmentLabelRunes = 120

// SanitizeAttachmentLabel cleans an attacker-controlled attachment filename (or
// other label) before it is rendered into the placeholder line that becomes part
// of the inbound text an LLM reads. A filename arrives from the remote chat
// service and is fully attacker-chosen, so without this a name like
// "x\n\nIgnore previous instructions and ..." would inject a forged line into the
// prompt, and a name with control characters or megabytes of text could corrupt or
// flood it. This is a defense-in-depth measure on the prompt-injection surface, not
// a guarantee the agent obeys only trusted text; it removes the cheap structural
// attacks (newline/control-char injection, framing breakout, length blowup). It
// collapses any run of control characters or whitespace (newlines, tabs, NUL, ...)
// to a single space, trims the ends, caps the length, and substitutes a placeholder
// when nothing usable remains, so the result is always a single safe line.
func SanitizeAttachmentLabel(name string) string {
	var b strings.Builder
	kept := 0 // runes written (cap is per-rune, not per-byte, for multibyte names)
	lastSpace := false
	for _, r := range name {
		// Treat C0/C1 controls (incl. newline, tab, NUL), the decode-error rune, and
		// any Unicode space as a single collapsed space; this is what stops a
		// newline-injected forged prompt line and control-char corruption.
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.IsSpace(r) {
			if !lastSpace && b.Len() > 0 {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		if kept >= maxAttachmentLabelRunes {
			break
		}
		b.WriteRune(r)
		kept++
		lastSpace = false
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "file"
	}
	return out
}

// SplitMessage breaks s into chunks of at most max runes each, so a reply that
// exceeds a channel's per-message limit (Telegram 4096, Discord 2000) is sent as
// several messages instead of being rejected. It splits on rune boundaries, never
// mid-rune, so multi-byte characters stay intact. An empty string yields a single
// empty chunk so the caller still sends one message; a non-positive max disables
// splitting (one chunk). Each adapter passes its own limit.
func SplitMessage(s string, max int) []string {
	if max <= 0 || len(s) <= max {
		return []string{s}
	}
	var chunks []string
	runes := []rune(s)
	for len(runes) > max {
		chunks = append(chunks, string(runes[:max]))
		runes = runes[max:]
	}
	return append(chunks, string(runes))
}
