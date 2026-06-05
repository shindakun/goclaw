// Package plugin adapts a channel PLUGIN (a kind=channel binary driven by
// internal/plugin.ChannelClient) onto the host's channels.ChannelAdapter, so the
// router and delivery loop treat a plugin channel exactly like a built-in one
// (Telegram, Discord). The adapter is the trusted host-side half: it maps the plugin's
// inbound stream to channels.InboundMsg, NAMESPACES the plugin-asserted identity so it
// cannot collide with another channel's owner id at the access gate, and forwards
// outbound replies to the plugin.
//
// The adapter is TRANSPORT-AGNOSTIC: it only needs a ChannelClient, not a process.
// That matters for the security model. In the real daemon the host MUST NOT run the
// plugin: untrusted downloaded plugin code runs in the agent container (the runner
// launches it, exactly as it launches tool plugins), and the host reaches it across the
// boundary. So the production wiring is the host CONNECTING to a sandboxed plugin over a
// socket, never `cmd/goclaw` exec-ing the plugin binary host-side.
//
// Today the only ChannelClient constructor (`plugin.LaunchChannel`) spawns a host child
// process; that path is for the chantest dev harness and tests, NOT the daemon. The
// sandboxed-boundary work splits the ChannelClient so it can attach to an
// already-connected socket instead of spawning, at which point this adapter wraps it
// unchanged. Until that lands, this package is wired ONLY by chantest/tests, never by
// cmd/goclaw.
package plugin

import (
	"context"

	"github.com/shindakun/goclaw/internal/channels"
	plug "github.com/shindakun/goclaw/internal/plugin"
)

// channelClient is the slice of *plug.ChannelClient this adapter needs. Narrowed to an
// interface so the adapter can be tested with a fake, without launching a real process.
type channelClient interface {
	Name() string
	Inbound() <-chan plug.ChannelInbound
	SendOutbound(ctx context.Context, out plug.ChannelOutbound) error
}

// Adapter wraps a channel plugin's ChannelClient as a channels.ChannelAdapter.
type Adapter struct {
	client channelClient
	name   string
}

// NewAdapter wraps a launched channel client. The adapter's Name() is the client's
// plugin name (e.g. "irc"), which is also the channel id the router keys on and the
// namespace prefix for plugin-asserted sender ids.
func NewAdapter(client channelClient) *Adapter {
	return &Adapter{client: client, name: client.Name()}
}

// Name returns the channel id (the plugin name).
func (a *Adapter) Name() string { return a.name }

// Start streams the plugin's inbound messages, mapped to channels.InboundMsg, until ctx
// is cancelled (or the plugin's inbound stream closes). It does NOT own the plugin
// process lifecycle: launching and Close-ing the ChannelClient is the caller's job. The
// returned channel is closed when the source closes or ctx is done.
func (a *Adapter) Start(ctx context.Context) (<-chan channels.InboundMsg, error) {
	out := make(chan channels.InboundMsg)
	src := a.client.Inbound()
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case in, ok := <-src:
				if !ok {
					return // plugin inbound stream closed
				}
				select {
				case out <- a.toInboundMsg(in):
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// Send forwards a reply to the plugin as an outbound message.
func (a *Adapter) Send(ctx context.Context, out channels.OutboundMsg) error {
	return a.client.SendOutbound(ctx, plug.ChannelOutbound{
		Channel: a.name,
		ChatID:  out.ChatID,
		Text:    out.Text,
	})
}

// toInboundMsg maps a plugin inbound to the router's InboundMsg. The Channel is forced
// to the adapter name (not whatever the plugin put in its payload), and the SenderID is
// NAMESPACED (see namespaceSenderID): the plugin asserts identity but is not trusted, so
// the host prefixes it so a plugin-asserted id can never equal another channel's owner
// id at the access gate. The host access gate still authorizes on top of this.
func (a *Adapter) toInboundMsg(in plug.ChannelInbound) channels.InboundMsg {
	return channels.InboundMsg{
		Channel:   a.name,
		ChatID:    in.ChatID,
		SenderID:  namespaceSenderID(a.name, in.SenderID),
		Sender:    in.Sender,
		Text:      in.Text,
		Timestamp: in.Timestamp,
	}
}

// namespaceSenderID prefixes a plugin-asserted sender id with the channel name, so an
// id a plugin controls (e.g. an unauthenticated IRC nick, which is spoofable) cannot
// collide with another channel's stable owner id (e.g. a Telegram numeric id) at the
// access gate. An already-prefixed id (the plugin chose to namespace) is left alone, so
// a plugin that follows the convention is not double-prefixed. An empty id stays empty
// (fail closed: the gate denies an empty/unknown sender).
func namespaceSenderID(channel, senderID string) string {
	if senderID == "" {
		return ""
	}
	prefix := channel + ":"
	if len(senderID) >= len(prefix) && senderID[:len(prefix)] == prefix {
		return senderID
	}
	return prefix + senderID
}
