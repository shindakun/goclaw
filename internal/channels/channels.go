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
	"time"
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
