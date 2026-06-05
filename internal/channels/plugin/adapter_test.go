package plugin

import (
	"context"
	"testing"
	"time"

	"github.com/shindakun/goclaw/internal/channels"
	plug "github.com/shindakun/goclaw/internal/plugin"
)

// fakeClient is a channelClient stand-in: a feedable inbound stream and a record of
// what SendOutbound was called with.
type fakeClient struct {
	name string
	in   chan plug.ChannelInbound
	sent []plug.ChannelOutbound
}

func newFakeClient(name string) *fakeClient {
	return &fakeClient{name: name, in: make(chan plug.ChannelInbound, 4)}
}

func (f *fakeClient) Name() string                        { return f.name }
func (f *fakeClient) Inbound() <-chan plug.ChannelInbound { return f.in }
func (f *fakeClient) SendOutbound(_ context.Context, o plug.ChannelOutbound) error {
	f.sent = append(f.sent, o)
	return nil
}

func TestAdapter_StartMapsInboundAndNamespacesSender(t *testing.T) {
	fc := newFakeClient("irc")
	a := NewAdapter(fc)
	if a.Name() != "irc" {
		t.Fatalf("Name() = %q, want irc", a.Name())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := a.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ts := time.Unix(1000, 0)
	fc.in <- plug.ChannelInbound{
		Channel:   "irc", // the plugin's claim; the adapter forces it to the adapter name
		ChatID:    "#goclawtester",
		SenderID:  "shindakun", // a bare, spoofable IRC nick
		Sender:    "shindakun",
		Text:      "ping?",
		Timestamp: ts,
	}

	select {
	case msg := <-stream:
		if msg.Channel != "irc" || msg.ChatID != "#goclawtester" || msg.Text != "ping?" {
			t.Fatalf("mapped msg = %+v", msg)
		}
		// The plugin-asserted sender id is NAMESPACED so it cannot collide with another
		// channel's owner id at the gate.
		if msg.SenderID != "irc:shindakun" {
			t.Fatalf("SenderID = %q, want irc:shindakun (namespaced)", msg.SenderID)
		}
		if msg.Sender != "shindakun" {
			t.Fatalf("Sender (display) = %q, want shindakun", msg.Sender)
		}
		if !msg.Timestamp.Equal(ts) {
			t.Fatalf("Timestamp = %v, want %v", msg.Timestamp, ts)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no inbound mapped through the adapter")
	}
}

func TestAdapter_SendForwardsToPlugin(t *testing.T) {
	fc := newFakeClient("irc")
	a := NewAdapter(fc)
	if err := a.Send(context.Background(), channels.OutboundMsg{ChatID: "#goclawtester", Text: "pong!"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(fc.sent) != 1 {
		t.Fatalf("plugin saw %d sends, want 1", len(fc.sent))
	}
	got := fc.sent[0]
	if got.Channel != "irc" || got.ChatID != "#goclawtester" || got.Text != "pong!" {
		t.Fatalf("forwarded outbound = %+v", got)
	}
}

func TestAdapter_StartClosesOnContextCancel(t *testing.T) {
	fc := newFakeClient("irc")
	a := NewAdapter(fc)
	ctx, cancel := context.WithCancel(context.Background())
	stream, _ := a.Start(ctx)
	cancel()
	select {
	case _, ok := <-stream:
		if ok {
			// drain to closed
			for range stream { //nolint:revive
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close on context cancel")
	}
}

func TestNamespaceSenderID(t *testing.T) {
	cases := []struct{ channel, in, want string }{
		{"irc", "shindakun", "irc:shindakun"},     // bare nick gets prefixed
		{"irc", "irc:shindakun", "irc:shindakun"}, // already-namespaced left alone
		{"irc", "", ""},                         // empty stays empty (gate fails closed)
		{"telegram", "12345", "telegram:12345"}, // different channel, different namespace
	}
	for _, c := range cases {
		if got := namespaceSenderID(c.channel, c.in); got != c.want {
			t.Errorf("namespaceSenderID(%q,%q) = %q, want %q", c.channel, c.in, got, c.want)
		}
	}
}
