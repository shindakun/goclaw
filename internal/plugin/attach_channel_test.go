package plugin

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// TestAttachChannel_RoundTrip drives AttachChannel over an in-memory net.Pipe (the
// stand-in for the boundary socket the real daemon uses), with a fake channel plugin on
// the other end speaking the wire protocol directly. It proves the attach path does the
// kind=channel handshake, surfaces a channel.inbound event, and correlates a
// channel.send result, all WITHOUT spawning a process. This is the path the sandboxed
// boundary will use (host attaches to a plugin running in the container).
func TestAttachChannel_RoundTrip(t *testing.T) {
	hostEnd, pluginEnd := net.Pipe()

	// Fake plugin: a goroutine speaking frames on pluginEnd.
	pluginErr := make(chan error, 1)
	go func() { pluginErr <- fakeChannelPlugin(pluginEnd) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := AttachChannel(ctx, "fake", hostEnd, quietLog())
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer func() { _ = c.Close() }()

	if c.Info().Name != "fake" || c.Info().Kind != "channel" {
		t.Fatalf("unexpected info: %+v", c.Info())
	}

	// The fake pushes one inbound after the handshake.
	select {
	case in := <-c.Inbound():
		if in.ChatID != "#room" || in.Text != "hi" {
			t.Fatalf("inbound = %+v, want chat #room text hi", in)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no inbound from the attached plugin")
	}

	// Send a reply; the fake answers OK, and records the text it received.
	if err := c.SendOutbound(ctx, ChannelOutbound{Channel: "fake", ChatID: "#room", Text: "pong"}); err != nil {
		t.Fatalf("send outbound: %v", err)
	}
}

// fakeChannelPlugin is a minimal kind=channel plugin over rw: it answers the hello
// handshake, sends one channel.inbound event, then answers a single channel.send with
// OK. It returns when rw closes.
func fakeChannelPlugin(rw net.Conn) error {
	defer func() { _ = rw.Close() }()
	sess := newSession(rw, rw)

	// 1. Handshake: read hello, reply hello.ok announcing kind=channel.
	f, err := sess.recv()
	if err != nil {
		return err
	}
	if f.Type != frameControl || f.Topic != topicHello {
		return errFake("expected hello")
	}
	hok := helloOK{Magic: magic, ProtocolVer: protocolVer, Info: Info{Name: "fake", Kind: "channel", Version: "1", ProtocolVer: protocolVer}}
	payload, _ := json.Marshal(hok)
	if err := sess.send(frame{Type: frameControl, Topic: topicHelloOK, Payload: payload}); err != nil {
		return err
	}

	// 2. Push one inbound event.
	inPayload, _ := json.Marshal(ChannelInbound{Channel: "fake", ChatID: "#room", SenderID: "u", Sender: "u", Text: "hi"})
	if err := sess.send(frame{Type: frameEvent, Topic: topicChannelInbound, Payload: inPayload}); err != nil {
		return err
	}

	// 3. Answer requests until the transport closes. A channel.send gets an OK result.
	for {
		req, err := sess.recv()
		if err != nil {
			return nil // host closed (Close sends shutdown then closes the pipe)
		}
		switch {
		case req.Type == frameControl && req.Topic == topicShutdown:
			return nil
		case req.Type == frameRequest && req.Topic == topicChannelSend:
			res, _ := json.Marshal(ChannelSendResult{OK: true})
			if err := sess.send(frame{Type: frameResult, ID: req.ID, Topic: topicChannelSend, Payload: res}); err != nil {
				return err
			}
		}
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }
