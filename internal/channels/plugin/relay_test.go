package plugin

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/shindakun/goclaw/internal/channels"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestRelay_OpenAttachesAndRoutes proves the host half end to end: the relay binds a
// socket, a fake "runner" dials in running a fake channel plugin, and Open returns an
// adapter whose Start surfaces an inbound (sender namespaced) and whose Send reaches the
// plugin. This exercises Relay -> AttachChannel -> Adapter over a real Unix socket.
func TestRelay_OpenAttachesAndRoutes(t *testing.T) {
	sockDir := t.TempDir()
	r, err := NewRelay(sockDir, quietLog())
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}
	defer r.CloseAll()

	// The fake runner dials the socket once it exists and speaks the plugin side.
	pluginSent := make(chan string, 1)
	go func() {
		sockPath := filepath.Join(sockDir, "irc.sock")
		conn := dialWhenReady(t, sockPath, 5*time.Second)
		if conn == nil {
			return
		}
		_ = fakeChannelPluginWire(conn, pluginSent)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	adapter, err := r.Open(ctx, "irc", 5*time.Second)
	if err != nil {
		t.Fatalf("relay open: %v", err)
	}
	if adapter.Name() != "irc" {
		t.Fatalf("adapter name = %q, want irc", adapter.Name())
	}

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

	// Close removes the channel and unlinks the socket.
	if !r.Close("irc") {
		t.Fatal("Close reported the channel was not open")
	}
	if _, err := net.Dial("unix", filepath.Join(sockDir, "irc.sock")); err == nil {
		t.Fatal("socket still connectable after Close")
	}
}

func TestRelay_RejectsBadName(t *testing.T) {
	r, _ := NewRelay(t.TempDir(), quietLog())
	ctx := context.Background()
	for _, bad := range []string{"", "../evil", "a/b", "x.sock"} {
		if _, err := r.Open(ctx, bad, time.Second); err == nil {
			t.Errorf("Open(%q) succeeded, want rejection", bad)
		}
	}
}

func TestRelay_OpenTimesOutIfNoDialer(t *testing.T) {
	r, _ := NewRelay(t.TempDir(), quietLog())
	ctx := context.Background()
	start := time.Now()
	if _, err := r.Open(ctx, "lonely", 200*time.Millisecond); err == nil {
		t.Fatal("Open with no dialer should time out")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("Open did not honor the timeout promptly")
	}
	// The socket must be cleaned up after a failed open.
	if _, err := net.Dial("unix", filepath.Join(r.sockDir, "lonely.sock")); err == nil {
		t.Fatal("socket left behind after a timed-out Open")
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

func dialWhenReady(t *testing.T, path string, timeout time.Duration) net.Conn {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", path); err == nil {
			return c
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

type errStr string

func (e errStr) Error() string { return string(e) }
