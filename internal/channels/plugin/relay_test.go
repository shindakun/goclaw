package plugin

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/shindakun/goclaw/internal/channels"
	plug "github.com/shindakun/goclaw/internal/plugin"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestRelay_OpenAttachesAndRoutes proves the host half end to end: the relay binds a
// socket, a fake "runner" dials in running a fake channel plugin, and Open returns an
// adapter whose Start surfaces an inbound (sender namespaced) and whose Send reaches the
// plugin. This exercises Relay -> AttachChannel -> Adapter over a real Unix socket.
func TestRelay_OpenAttachesAndRoutes(t *testing.T) {
	// Use the TCP transport: it is the path that works on macOS, and the endpoint Addr is
	// a real bind address the fake runner can dial (the unix endpoint path is a container
	// path that does not exist in a native test).
	pdir := t.TempDir()
	r, err := NewRelay(Config{Transport: TransportTCP, TCPHost: "127.0.0.1", TCPBind: "127.0.0.1"}, quietLog())
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}
	defer r.CloseAll()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Open returns immediately (the plugin dials in the background); inbound flows once
	// the fake runner reads .endpoint, dials, sends the token, and handshakes.
	adapter, err := r.Open("irc", pdir)
	if err != nil {
		t.Fatalf("relay open: %v", err)
	}
	if adapter.Name() != "irc" {
		t.Fatalf("adapter name = %q, want irc", adapter.Name())
	}

	// The fake runner reads the host-written .endpoint, dials TCP, sends the token line,
	// then speaks the plugin side of the protocol.
	pluginSent := make(chan string, 1)
	go func() {
		ep, derr := plug.ReadChannelEndpoint(pdir)
		if derr != nil {
			return
		}
		conn := dialTCPWhenReady(t, ep.Addr, 5*time.Second)
		if conn == nil {
			return
		}
		_, _ = conn.Write([]byte(ep.Token + "\n")) // token line first
		_ = fakeChannelPluginWire(conn, pluginSent)
	}()

	stream, err := adapter.Start(ctx)
	if err != nil {
		t.Fatalf("adapter start: %v", err)
	}

	// The fake plugin pushes one inbound; the adapter maps + namespaces it.
	select {
	case msg := <-stream:
		if msg.Channel != "irc" || msg.ChatID != "#room" || msg.Text != "hi" {
			t.Fatalf("inbound msg = %+v", msg)
		}
		if msg.SenderID != "irc:bob" {
			t.Fatalf("SenderID = %q, want irc:bob (namespaced)", msg.SenderID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no inbound routed through the relay")
	}

	// Send a reply; it should reach the plugin as the text we sent.
	if err := adapter.Send(ctx, channels.OutboundMsg{ChatID: "#room", Text: "pong"}); err != nil {
		t.Fatalf("adapter send: %v", err)
	}
	select {
	case got := <-pluginSent:
		if got != "pong" {
			t.Fatalf("plugin received %q, want pong", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("plugin never received the outbound")
	}

	// Close removes the channel and stops listening (the endpoint addr stops accepting).
	ep, _ := plug.ReadChannelEndpoint(pdir)
	if !r.Close("irc") {
		t.Fatal("Close reported the channel was not open")
	}
	if c, err := net.DialTimeout("tcp", ep.Addr, 500*time.Millisecond); err == nil {
		_ = c.Close()
		t.Fatal("endpoint still accepting after Close")
	}
}

func TestRelay_RejectsBadName(t *testing.T) {
	r, _ := NewRelay(Config{Transport: TransportTCP, TCPHost: "127.0.0.1", TCPBind: "127.0.0.1"}, quietLog())
	for _, bad := range []string{"", "../evil", "a/b", "x.sock"} {
		if _, err := r.Open(bad, t.TempDir()); err == nil {
			t.Errorf("Open(%q) succeeded, want rejection", bad)
		}
	}
}

// Open returns immediately even with no dialer (the container launches lazily). Until a
// plugin attaches, Send errors with "not connected".
func TestRelay_OpenReturnsBeforeDialAndSendErrorsUntilAttached(t *testing.T) {
	r, _ := NewRelay(Config{Transport: TransportTCP, TCPHost: "127.0.0.1", TCPBind: "127.0.0.1"}, quietLog())
	defer r.CloseAll()

	start := time.Now()
	adapter, err := r.Open("lonely", t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("Open blocked; it must return before any dial")
	}

	// No plugin attached yet: Send fails closed.
	if err := adapter.Send(context.Background(), channels.OutboundMsg{ChatID: "#x", Text: "hi"}); err == nil {
		t.Fatal("Send before any plugin attached should error")
	}

	if !r.Close("lonely") {
		t.Fatal("Close reported not open")
	}
}

// --- minimal channel-plugin wire implementation (host-facing peer) ---

const (
	wireMagic     = "GCLW"
	wireVer       = 1
	fControl      = 0
	fRequest      = 1
	fResult       = 2
	fEvent        = 3
	tHello        = "hello"
	tHelloOK      = "hello.ok"
	tShutdown     = "shutdown"
	tChanInbound  = "channel.inbound"
	tChanSend     = "channel.send"
	wireHeaderLen = 17
)

// fakeChannelPluginWire speaks the plugin side of the protocol over conn: handshake as
// kind=channel, push one inbound, then answer one channel.send (recording its text on
// sent) until the connection closes.
func fakeChannelPluginWire(conn net.Conn, sent chan<- string) error {
	defer func() { _ = conn.Close() }()

	// Handshake.
	f, err := readWire(conn)
	if err != nil {
		return err
	}
	if f.topic != tHello {
		return errStr("expected hello")
	}
	hok := map[string]any{
		"magic": wireMagic, "protocol_ver": wireVer,
		"info": map[string]any{"name": "irc", "kind": "channel", "version": "1", "protocol_ver": wireVer},
	}
	hokB, _ := json.Marshal(hok)
	if err := writeWire(conn, fControl, 0, tHelloOK, hokB); err != nil {
		return err
	}

	// One inbound event (SenderID is a bare nick; the adapter namespaces it).
	inB, _ := json.Marshal(map[string]any{"channel": "irc", "chat_id": "#room", "sender_id": "bob", "sender": "bob", "text": "hi"})
	if err := writeWire(conn, fEvent, 0, tChanInbound, inB); err != nil {
		return err
	}

	// Answer requests.
	for {
		req, err := readWire(conn)
		if err != nil {
			return nil
		}
		switch {
		case req.ftype == fControl && req.topic == tShutdown:
			return nil
		case req.ftype == fRequest && req.topic == tChanSend:
			var out struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(req.payload, &out)
			select {
			case sent <- out.Text:
			default:
			}
			resB, _ := json.Marshal(map[string]any{"ok": true})
			if err := writeWire(conn, fResult, req.id, tChanSend, resB); err != nil {
				return err
			}
		}
	}
}

type wireFrame struct {
	ftype   uint8
	id      uint64
	topic   string
	payload []byte
}

func writeWire(w io.Writer, ftype uint8, id uint64, topic string, payload []byte) error {
	var hdr [wireHeaderLen]byte
	copy(hdr[0:4], wireMagic)
	hdr[4] = wireVer
	hdr[5] = ftype
	hdr[6] = 0
	binary.BigEndian.PutUint64(hdr[7:15], id)
	binary.BigEndian.PutUint16(hdr[15:17], uint16(len(topic)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := io.WriteString(w, topic); err != nil {
		return err
	}
	var pl [4]byte
	binary.BigEndian.PutUint32(pl[:], uint32(len(payload)))
	if _, err := w.Write(pl[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readWire(r io.Reader) (wireFrame, error) {
	var hdr [wireHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return wireFrame{}, err
	}
	f := wireFrame{ftype: hdr[5], id: binary.BigEndian.Uint64(hdr[7:15])}
	topicLen := binary.BigEndian.Uint16(hdr[15:17])
	tb := make([]byte, topicLen)
	if _, err := io.ReadFull(r, tb); err != nil {
		return wireFrame{}, err
	}
	f.topic = string(tb)
	var pl [4]byte
	if _, err := io.ReadFull(r, pl[:]); err != nil {
		return wireFrame{}, err
	}
	payLen := binary.BigEndian.Uint32(pl[:])
	if payLen > 0 {
		f.payload = make([]byte, payLen)
		if _, err := io.ReadFull(r, f.payload); err != nil {
			return wireFrame{}, err
		}
	}
	return f, nil
}

func dialTCPWhenReady(t *testing.T, addr string, timeout time.Duration) net.Conn {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", addr); err == nil {
			return c
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

type errStr string

func (e errStr) Error() string { return string(e) }
