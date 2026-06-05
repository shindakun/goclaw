package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	plug "github.com/shindakun/goclaw/internal/plugin"
)

// Relay is the host side of the channel boundary. For each channel plugin it binds a
// per-channel Unix socket in a shared dir (mounted into the container at
// /run/goclaw/channels), waits for the in-container runner to dial in, then drives the
// framed channel.* protocol over that connection via plug.AttachChannel and exposes the
// result as a channels.ChannelAdapter.
//
// The relay is the TRUSTED half: it never runs the plugin (the runner launches it in the
// sandbox and dials out to us). The relay only listens, attaches, and namespaces
// identity. One Relay owns the socket dir; Open is called once per channel plugin.
type Relay struct {
	sockDir string
	log     *slog.Logger

	mu   sync.Mutex
	open map[string]*openChannel // by channel name
}

// openChannel is one live channel: its listener, the attached client, and the adapter.
type openChannel struct {
	name     string
	listener net.Listener
	sockPath string
	client   *plug.ChannelClient
	adapter  *Adapter
}

// NewRelay creates a relay that binds per-channel sockets under sockDir (the host side
// of the /run/goclaw/channels mount). The dir is created if absent.
func NewRelay(sockDir string, log *slog.Logger) (*Relay, error) {
	if err := os.MkdirAll(sockDir, 0o755); err != nil {
		return nil, fmt.Errorf("relay: create socket dir %q: %w", sockDir, err)
	}
	return &Relay{sockDir: sockDir, log: log, open: map[string]*openChannel{}}, nil
}

// Open binds the channel's socket, waits (up to timeout) for the in-container runner to
// dial in, performs the channel handshake, and returns a ready ChannelAdapter the caller
// registers with the channels.Registry. The plugin must run kind=channel or Open fails.
//
// Order is forgiving: the relay listens first; the runner retries its dial until we
// accept, so Open can be called before or after the container launches the plugin.
func (r *Relay) Open(ctx context.Context, name string, timeout time.Duration) (*Adapter, error) {
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
	// Remove a stale socket file so Listen does not fail with "address already in use".
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("relay: listen %q: %w", sockPath, err)
	}
	// Tight perms: only the host user should reach this socket.
	_ = os.Chmod(sockPath, 0o600)

	conn, err := acceptWithTimeout(ctx, ln, timeout)
	if err != nil {
		_ = ln.Close()
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("relay: channel %q: %w", name, err)
	}

	client, err := plug.AttachChannel(ctx, name, conn, r.log)
	if err != nil {
		_ = ln.Close()
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("relay: channel %q attach: %w", name, err)
	}

	oc := &openChannel{
		name:     name,
		listener: ln,
		sockPath: sockPath,
		client:   client,
		adapter:  NewAdapter(client),
	}
	r.mu.Lock()
	r.open[name] = oc
	r.mu.Unlock()
	r.log.Info("channel relay open", "channel", name, "sock", sockPath)
	return oc.adapter, nil
}

// Close tears down one open channel: close the client (sends shutdown), stop listening,
// and unlink the socket. Returns whether the channel was open. The caller is responsible
// for unregistering the adapter from the channels.Registry first.
func (r *Relay) Close(name string) bool {
	r.mu.Lock()
	oc, ok := r.open[name]
	if ok {
		delete(r.open, name)
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	_ = oc.client.Close()
	_ = oc.listener.Close()
	_ = os.Remove(oc.sockPath)
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

// acceptWithTimeout accepts one connection, honoring ctx and a deadline. It runs Accept
// in a goroutine because net.Listener has no context-aware Accept; closing the listener
// on timeout unblocks it.
func acceptWithTimeout(ctx context.Context, ln net.Listener, timeout time.Duration) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := ln.Accept()
		ch <- result{c, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res.conn, res.err
	case <-timer.C:
		_ = ln.Close() // unblocks the Accept goroutine (it returns an error we drop)
		return nil, fmt.Errorf("timed out waiting for the in-container plugin to connect")
	case <-ctx.Done():
		_ = ln.Close()
		return nil, ctx.Err()
	}
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
