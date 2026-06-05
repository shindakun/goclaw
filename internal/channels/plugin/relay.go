package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"

	plug "github.com/shindakun/goclaw/internal/plugin"
)

// Relay is the host side of the channel boundary. For each channel plugin it binds a
// per-channel Unix socket in a shared dir (mounted into the container at
// /run/goclaw/channels) and accepts the in-container runner's dial IN THE BACKGROUND,
// then drives the framed channel.* protocol over that connection via plug.AttachChannel.
//
// The relay is the TRUSTED half: it never runs the plugin (the runner launches it in the
// sandbox and dials out to us). Open returns a channels.ChannelAdapter IMMEDIATELY,
// before any dial: the container launches lazily (on the first message), so the relay
// must already be listening when the runner finally dials. The adapter's inbound stream
// is durable, fed once the plugin attaches and across re-attach if the container is
// recreated.
type Relay struct {
	sockDir string
	log     *slog.Logger

	mu   sync.Mutex
	open map[string]*relayChannel // by channel name
}

// NewRelay creates a relay that binds per-channel sockets under sockDir (the host side
// of the /run/goclaw/channels mount). The dir is created if absent.
func NewRelay(sockDir string, log *slog.Logger) (*Relay, error) {
	if err := os.MkdirAll(sockDir, 0o755); err != nil {
		return nil, fmt.Errorf("relay: create socket dir %q: %w", sockDir, err)
	}
	return &Relay{sockDir: sockDir, log: log, open: map[string]*relayChannel{}}, nil
}

// Open binds the channel's socket and starts accepting the in-container runner's dial in
// the background, then returns a ChannelAdapter ready to register with the
// channels.Registry. It does NOT block waiting for the plugin to connect: inbound flows
// once the runner dials in (after the container launches on first message).
func (r *Relay) Open(name string) (*Adapter, error) {
	name = sanitizeChannelName(name)
	if name == "" {
		return nil, fmt.Errorf("relay: invalid channel name")
	}

	r.mu.Lock()
	if _, exists := r.open[name]; exists {
		r.mu.Unlock()
		return nil, fmt.Errorf("relay: channel %q already open", name)
	}
	r.mu.Unlock()

	sockPath := filepath.Join(r.sockDir, name+".sock")
	_ = os.Remove(sockPath) // clear a stale socket so Listen does not EADDRINUSE
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("relay: listen %q: %w", sockPath, err)
	}
	_ = os.Chmod(sockPath, 0o600) // host user only

	rc := &relayChannel{
		name:     name,
		log:      r.log,
		listener: ln,
		sockPath: sockPath,
		inbound:  make(chan plug.ChannelInbound),
		done:     make(chan struct{}),
	}
	r.mu.Lock()
	r.open[name] = rc
	r.mu.Unlock()

	go rc.acceptLoop()
	r.log.Info("channel relay listening", "channel", name, "sock", sockPath)
	return NewAdapter(rc), nil
}

// Close tears down one open channel: stop accepting, close any attached client, unlink
// the socket. Returns whether the channel was open. The caller unregisters the adapter
// from the channels.Registry first.
func (r *Relay) Close(name string) bool {
	r.mu.Lock()
	rc, ok := r.open[name]
	if ok {
		delete(r.open, name)
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	rc.stop()
	r.log.Info("channel relay closed", "channel", name)
	return true
}

// CloseAll tears down every open channel (host shutdown).
func (r *Relay) CloseAll() {
	r.mu.Lock()
	names := make([]string, 0, len(r.open))
	for n := range r.open {
		names = append(names, n)
	}
	r.mu.Unlock()
	for _, n := range names {
		r.Close(n)
	}
}

// relayChannel is one channel's durable host endpoint. It implements channelClient (so
// an Adapter wraps it): a durable inbound stream and a Send that forwards to whatever
// client is currently attached. It owns the listener and accepts the runner's dial,
// re-accepting if the container is recreated, so the adapter survives a container bounce.
type relayChannel struct {
	name     string
	log      *slog.Logger
	listener net.Listener
	sockPath string

	inbound chan plug.ChannelInbound // durable; fed by each attached client
	done    chan struct{}            // closed by stop()

	mu      sync.Mutex
	client  *plug.ChannelClient // the currently-attached client (nil until first dial)
	stopped bool
}

func (rc *relayChannel) Name() string                        { return rc.name }
func (rc *relayChannel) Inbound() <-chan plug.ChannelInbound { return rc.inbound }

// SendOutbound forwards to the currently-attached client, or errors if none is attached
// (the container has not dialed in yet, or dropped).
func (rc *relayChannel) SendOutbound(ctx context.Context, out plug.ChannelOutbound) error {
	rc.mu.Lock()
	c := rc.client
	rc.mu.Unlock()
	if c == nil {
		return fmt.Errorf("channel %q: not connected (plugin has not dialed in)", rc.name)
	}
	return c.SendOutbound(ctx, out)
}

// acceptLoop accepts the runner's dial, attaches, and pumps that client's inbound into
// the durable stream. When the client ends (container bounce), it loops to accept the
// next dial, until stop().
func (rc *relayChannel) acceptLoop() {
	for {
		conn, err := rc.listener.Accept()
		if err != nil {
			return // listener closed by stop()
		}
		client, err := plug.AttachChannel(context.Background(), rc.name, conn, rc.log)
		if err != nil {
			rc.log.Warn("channel relay: attach failed", "channel", rc.name, "err", err)
			_ = conn.Close()
			select {
			case <-rc.done:
				return
			default:
				continue
			}
		}
		rc.mu.Lock()
		if rc.stopped {
			rc.mu.Unlock()
			_ = client.Close()
			return
		}
		rc.client = client
		rc.mu.Unlock()
		rc.log.Info("channel plugin attached", "channel", rc.name)

		// Pump this client's inbound into the durable stream until it closes.
		for in := range client.Inbound() {
			select {
			case rc.inbound <- in:
			case <-rc.done:
				_ = client.Close()
				return
			}
		}
		// Client ended (container bounced or shut down). Drop it and re-accept.
		rc.mu.Lock()
		rc.client = nil
		rc.mu.Unlock()
		select {
		case <-rc.done:
			return
		default:
			rc.log.Info("channel plugin detached; awaiting re-dial", "channel", rc.name)
		}
	}
}

func (rc *relayChannel) stop() {
	rc.mu.Lock()
	if rc.stopped {
		rc.mu.Unlock()
		return
	}
	rc.stopped = true
	c := rc.client
	rc.mu.Unlock()

	close(rc.done)
	if c != nil {
		_ = c.Close()
	}
	_ = rc.listener.Close()
	_ = os.Remove(rc.sockPath)
}

// sanitizeChannelName guards the socket filename against path traversal from a plugin
// name. A channel name is a plugin name (already constrained), but the relay turns it
// into a filesystem path, so it re-validates: reject anything with a separator or dot.
func sanitizeChannelName(name string) string {
	if name == "" || filepath.Base(name) != name {
		return ""
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			// ok
		default:
			return ""
		}
	}
	return name
}
