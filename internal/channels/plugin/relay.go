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

// Transport selects how the host relay and the in-container runner connect.
type Transport string

// channelSockContainerPath is where the host's channel-socket dir is mounted inside the
// container. Must match internal/runtime's channelSockMountPath and cmd/claude-runner's
// channelSocketDir. Kept as a local constant to avoid importing runtime (cycle risk).
const channelSockContainerPath = "/run/goclaw/channels"

const (
	// TransportUnix: a per-channel Unix socket in a dir mounted into the container. Works
	// on native Linux (shared kernel) but NOT across the macOS<->podman-VM virtiofs share
	// (connect returns "operation not supported").
	TransportUnix Transport = "unix"
	// TransportTCP: the host binds a TCP listener and the container dials it at a
	// host-reachable address (e.g. host.docker.internal:PORT). Works on macOS and Linux.
	// A per-channel token guards the more-exposed TCP port.
	TransportTCP Transport = "tcp"
)

// Relay is the host side of the channel boundary. For each channel plugin it binds a
// per-channel listener (Unix socket or TCP), writes a .endpoint file into the channel's
// plugin dir telling the in-container runner how to dial back, and accepts that dial in
// the background to drive the framed channel.* protocol via plug.AttachChannel.
//
// The relay is the TRUSTED half: it never runs the plugin (the runner launches it in the
// sandbox and dials out to us). Open returns a channels.ChannelAdapter IMMEDIATELY,
// before any dial: the container launches lazily, so the relay must already be listening
// when the runner finally dials. The adapter's inbound stream is durable across re-attach.
type Relay struct {
	transport Transport
	sockDir   string // unix: dir for per-channel sockets (host side of the mount)
	tcpHost   string // tcp: the host address the CONTAINER dials (e.g. "host.docker.internal")
	tcpBind   string // tcp: the address the host BINDS (e.g. "0.0.0.0"); port is chosen per channel
	log       *slog.Logger

	mu   sync.Mutex
	open map[string]*relayChannel // by channel name
}

// Config configures a Relay's transport.
type Config struct {
	Transport Transport
	// Unix:
	SockDir string
	// TCP: TCPHost is the host address the container dials (host.docker.internal); TCPBind
	// is what the host listens on (default 0.0.0.0 so the podman VM can reach it).
	TCPHost string
	TCPBind string
}

// NewRelay creates a relay for the configured transport.
func NewRelay(cfg Config, log *slog.Logger) (*Relay, error) {
	r := &Relay{transport: cfg.Transport, log: log, open: map[string]*relayChannel{}}
	switch cfg.Transport {
	case TransportUnix:
		if cfg.SockDir == "" {
			return nil, fmt.Errorf("relay: unix transport needs a SockDir")
		}
		if err := os.MkdirAll(cfg.SockDir, 0o755); err != nil {
			return nil, fmt.Errorf("relay: create socket dir %q: %w", cfg.SockDir, err)
		}
		r.sockDir = cfg.SockDir
	case TransportTCP:
		if cfg.TCPHost == "" {
			return nil, fmt.Errorf("relay: tcp transport needs a TCPHost (e.g. host.docker.internal)")
		}
		r.tcpHost = cfg.TCPHost
		r.tcpBind = cfg.TCPBind
		if r.tcpBind == "" {
			r.tcpBind = "0.0.0.0"
		}
	default:
		return nil, fmt.Errorf("relay: unknown transport %q", cfg.Transport)
	}
	return r, nil
}

// Open binds the channel's listener, writes the .endpoint file into pluginHostDir (so the
// runner, which sees that dir mounted into the container, can read it and dial back),
// starts accepting the runner's dial in the background, and returns a ChannelAdapter to
// register. It does NOT block waiting for the plugin to connect.
func (r *Relay) Open(name, pluginHostDir string) (*Adapter, error) {
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

	ln, ep, cleanup, err := r.listen(name)
	if err != nil {
		return nil, err
	}
	if err := plug.WriteChannelEndpoint(pluginHostDir, ep); err != nil {
		_ = ln.Close()
		cleanup()
		return nil, fmt.Errorf("relay: write endpoint: %w", err)
	}

	rc := &relayChannel{
		name:          name,
		log:           r.log,
		listener:      ln,
		token:         ep.Token,
		pluginHostDir: pluginHostDir,
		cleanup:       cleanup,
		inbound:       make(chan plug.ChannelInbound),
		done:          make(chan struct{}),
	}
	r.mu.Lock()
	r.open[name] = rc
	r.mu.Unlock()

	go rc.acceptLoop()
	r.log.Info("channel relay listening", "channel", name, "transport", string(r.transport), "endpoint", ep.LogString())
	return NewAdapter(rc), nil
}

// listen binds the per-channel listener and returns it, the endpoint to advertise, and a
// cleanup that removes any on-disk artifact (the unix socket file).
func (r *Relay) listen(name string) (net.Listener, plug.ChannelEndpoint, func(), error) {
	switch r.transport {
	case TransportUnix:
		sockPath := filepath.Join(r.sockDir, name+".sock")
		_ = os.Remove(sockPath) // clear a stale socket so Listen does not EADDRINUSE
		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			return nil, plug.ChannelEndpoint{}, nil, fmt.Errorf("relay: listen %q: %w", sockPath, err)
		}
		_ = os.Chmod(sockPath, 0o600) // host user only
		// The runner sees the socket at the in-container channel-socket mount path, not the
		// host path. sockDir is mounted at channelSockContainerPath in the container.
		ep := plug.ChannelEndpoint{Transport: "unix", Path: channelSockContainerPath + "/" + name + ".sock"}
		return ln, ep, func() { _ = os.Remove(sockPath) }, nil
	case TransportTCP:
		ln, err := net.Listen("tcp", net.JoinHostPort(r.tcpBind, "0"))
		if err != nil {
			return nil, plug.ChannelEndpoint{}, nil, fmt.Errorf("relay: tcp listen: %w", err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		token, err := plug.NewChannelToken()
		if err != nil {
			_ = ln.Close()
			return nil, plug.ChannelEndpoint{}, nil, fmt.Errorf("relay: token: %w", err)
		}
		ep := plug.ChannelEndpoint{
			Transport: "tcp",
			Addr:      net.JoinHostPort(r.tcpHost, fmt.Sprintf("%d", port)),
			Token:     token,
		}
		return ln, ep, func() {}, nil
	default:
		return nil, plug.ChannelEndpoint{}, nil, fmt.Errorf("relay: unknown transport %q", r.transport)
	}
}

// Close tears down one open channel. Returns whether the channel was open. The caller
// unregisters the adapter from the channels.Registry first.
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

// OpenCount returns how many channels are currently open (registered). The host uses a
// non-zero count to decide it should eagerly launch a runner container at startup, so a
// channel plugin connects to its upstream immediately instead of waiting for a first
// unrelated message.
func (r *Relay) OpenCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.open)
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

// relayChannel is one channel's durable host endpoint. It implements channelClient (so an
// Adapter wraps it): a durable inbound stream and a Send that forwards to whatever client
// is currently attached. It owns the listener and accepts the runner's dial, re-accepting
// if the container is recreated, so the adapter survives a container bounce.
type relayChannel struct {
	name          string
	log           *slog.Logger
	listener      net.Listener
	token         string // tcp: required leading token; "" for unix
	pluginHostDir string // where the .endpoint file was written (removed on stop)
	cleanup       func() // transport teardown (remove unix socket file)

	inbound chan plug.ChannelInbound // durable; fed by each attached client
	done    chan struct{}            // closed by stop()

	mu      sync.Mutex
	client  *plug.ChannelClient // the currently-attached client (nil until first dial)
	stopped bool
}

func (rc *relayChannel) Name() string                        { return rc.name }
func (rc *relayChannel) Inbound() <-chan plug.ChannelInbound { return rc.inbound }

// SendOutbound forwards to the currently-attached client, or errors if none is attached.
func (rc *relayChannel) SendOutbound(ctx context.Context, out plug.ChannelOutbound) error {
	rc.mu.Lock()
	c := rc.client
	rc.mu.Unlock()
	if c == nil {
		return fmt.Errorf("channel %q: not connected (plugin has not dialed in)", rc.name)
	}
	return c.SendOutbound(ctx, out)
}

// acceptLoop accepts the runner's dial, verifies the token (tcp), attaches, and pumps that
// client's inbound into the durable stream. On client end (container bounce) it re-accepts.
func (rc *relayChannel) acceptLoop() {
	for {
		conn, err := rc.listener.Accept()
		if err != nil {
			return // listener closed by stop()
		}
		client, err := rc.attach(conn)
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

		for in := range client.Inbound() {
			select {
			case rc.inbound <- in:
			case <-rc.done:
				_ = client.Close()
				return
			}
		}
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

// attach turns an accepted connection into a ChannelClient. For tcp it first reads and
// verifies the leading token line (fail closed on mismatch), then resumes the framed
// protocol on the buffered reader so no bytes are lost.
func (rc *relayChannel) attach(conn net.Conn) (*plug.ChannelClient, error) {
	if rc.token != "" {
		br, err := plug.ReadAndCheckToken(conn, rc.token)
		if err != nil {
			return nil, err
		}
		return plug.AttachChannelReader(context.Background(), rc.name, br, conn, conn.Close, rc.log)
	}
	return plug.AttachChannel(context.Background(), rc.name, conn, rc.log)
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
	rc.cleanup()
	plug.RemoveChannelEndpoint(rc.pluginHostDir)
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
