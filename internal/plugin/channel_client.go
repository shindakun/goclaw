package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"sync/atomic"
)

// ChannelClient drives one running CHANNEL plugin process (kind: channel). It is the
// channel-side counterpart to Client (which drives tool plugins): it launches the
// binary, runs the handshake asserting kind=channel, then runs a single read loop that
// turns channel.inbound events into a Go channel the host can range over, and turns
// SendOutbound calls into channel.send request frames whose results it correlates back
// by frame ID.
//
// This is the host half of goclawkit's ServeChannel. The plugin speaks the framed
// protocol over its stdio; ChannelClient speaks it from the other end. It does NOT yet
// involve a container or a socket boundary: for initial connectivity testing the host
// launches the binary directly. The sandboxed-relay-over-socket path is a later layer
// that reuses this same client.
type ChannelClient struct {
	name string
	// shutdown tears down the underlying transport: for a spawned process it kills and
	// reaps it (LaunchChannel); for an attached stream it closes the connection
	// (AttachChannel). Decoupling this from a *exec.Cmd is what lets the SAME client
	// drive a plugin over either a host child's stdio OR a socket to a sandboxed plugin.
	shutdown func() error
	sess     *session
	log      *slog.Logger

	info Info // filled by the handshake

	inbound chan ChannelInbound // channel.inbound events, closed when the read loop ends

	nextID atomic.Uint64

	mu      sync.Mutex
	pending map[uint64]chan ChannelSendResult // id -> waiter for a channel.send result
	closed  bool
	loopErr error
	done    chan struct{}
}

// LaunchChannel starts the channel plugin binary at execPath, wires its stdio, performs
// the handshake (requiring kind=channel), and returns a ready ChannelClient with its
// inbound stream already pumping. env is the full process environment (the caller is
// responsible for having injected any IRC_*/config values the manifest's env list
// names). The returned client runs a background read loop until Close or process exit.
func LaunchChannel(ctx context.Context, name, execPath string, env []string, log *slog.Logger) (*ChannelClient, error) {
	cmd := exec.CommandContext(ctx, execPath)
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("channel %q: stdin pipe: %w", name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("channel %q: stdout pipe: %w", name, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("channel %q: stderr pipe: %w", name, err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("channel %q: start %s: %w", name, execPath, err)
	}

	// shutdown for a spawned process: kill and reap it.
	shutdown := func() error {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return cmd.Wait()
	}
	c := newChannelClient(name, stdout, stdin, shutdown, log)
	go c.drainStderr(stderr)

	if err := c.start(); err != nil {
		return nil, err
	}
	return c, nil
}

// AttachChannel drives a channel plugin over an ALREADY-CONNECTED stream (rw), instead
// of spawning a process. This is the path the real daemon uses: the plugin runs in the
// container and the host attaches to it across the boundary (a Unix socket), so the host
// never exec-s the plugin binary. rw carries the framed protocol both ways; on shutdown
// the client closes it. Performs the same handshake (requiring kind=channel) and starts
// the same read loop as LaunchChannel.
func AttachChannel(ctx context.Context, name string, rw io.ReadWriteCloser, log *slog.Logger) (*ChannelClient, error) {
	return AttachChannelReader(ctx, name, rw, rw, rw.Close, log)
}

// AttachChannelReader is AttachChannel with an explicit reader, writer, and closer, so a
// caller that has ALREADY consumed a prefix of the stream (e.g. a tcp endpoint reading a
// leading auth-token line into a bufio.Reader) can hand that buffered reader in without
// losing the buffered bytes. r and w are typically the same conn; closer tears it down.
func AttachChannelReader(ctx context.Context, name string, r io.Reader, w io.Writer, closer func() error, log *slog.Logger) (*ChannelClient, error) {
	c := newChannelClient(name, r, w, closer, log)
	if err := c.start(); err != nil {
		return nil, err
	}
	return c, nil
}

// newChannelClient builds a client over a reader/writer pair with a transport-specific
// shutdown. It does NOT handshake or loop; call start() for that.
func newChannelClient(name string, r io.Reader, w io.Writer, shutdown func() error, log *slog.Logger) *ChannelClient {
	return &ChannelClient{
		name:     name,
		shutdown: shutdown,
		sess:     newSession(r, w),
		log:      log,
		inbound:  make(chan ChannelInbound),
		pending:  make(map[uint64]chan ChannelSendResult),
		done:     make(chan struct{}),
	}
}

// start runs the handshake and, on success, launches the read loop. On a handshake
// failure it tears the transport down.
func (c *ChannelClient) start() error {
	if err := c.handshake(); err != nil {
		_ = c.shutdown()
		return err
	}
	go c.readLoop()
	return nil
}

// Info returns the plugin's announced identity.
func (c *ChannelClient) Info() Info { return c.info }

// Name returns the plugin name.
func (c *ChannelClient) Name() string { return c.name }

// Inbound returns the stream of inbound messages the plugin pushes up. It is closed
// when the read loop stops (process exit or transport error).
func (c *ChannelClient) Inbound() <-chan ChannelInbound { return c.inbound }

// handshake sends hello and waits for a matching hello.ok, requiring the plugin to
// announce kind=channel. It runs before the read loop, so it reads the reply directly.
func (c *ChannelClient) handshake() error {
	payload, _ := json.Marshal(hello{Magic: magic, ProtocolVer: protocolVer})
	if err := c.sess.send(frame{Type: frameControl, Topic: topicHello, Payload: payload}); err != nil {
		return fmt.Errorf("channel %q: send hello: %w", c.name, err)
	}
	f, err := c.sess.recv()
	if err != nil {
		return fmt.Errorf("channel %q: read hello.ok: %w", c.name, err)
	}
	if f.Type != frameControl || f.Topic != topicHelloOK {
		return fmt.Errorf("channel %q: expected hello.ok, got type=%d topic=%q", c.name, f.Type, f.Topic)
	}
	var hok helloOK
	if err := json.Unmarshal(f.Payload, &hok); err != nil {
		return fmt.Errorf("channel %q: bad hello.ok payload: %w", c.name, err)
	}
	if hok.Magic != magic || hok.ProtocolVer != protocolVer {
		return fmt.Errorf("channel %q: handshake mismatch: plugin magic=%q ver=%d, host magic=%q ver=%d",
			c.name, hok.Magic, hok.ProtocolVer, magic, protocolVer)
	}
	if hok.Info.Kind != "channel" {
		return fmt.Errorf("channel %q: plugin announced kind=%q, want channel", c.name, hok.Info.Kind)
	}
	c.info = hok.Info
	return nil
}

// SendOutbound writes a channel.send request and waits for its correlated result. A
// plugin-reported send error is returned as a non-nil error.
func (c *ChannelClient) SendOutbound(ctx context.Context, out ChannelOutbound) error {
	id := c.nextID.Add(1)
	ch := make(chan ChannelSendResult, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("channel %q: closed (%v)", c.name, c.loopErr)
	}
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	payload, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("channel %q: marshal outbound: %w", c.name, err)
	}
	if err := c.sess.send(frame{Type: frameRequest, ID: id, Topic: topicChannelSend, Payload: payload}); err != nil {
		return fmt.Errorf("channel %q: send outbound: %w", c.name, err)
	}

	select {
	case res := <-ch:
		if !res.OK {
			return fmt.Errorf("channel %q send: %s", c.name, res.Error)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return fmt.Errorf("channel %q: process ended before send result (%v)", c.name, c.loopErr)
	}
}

// readLoop reads frames until the transport closes. channel.inbound events are pushed
// onto the inbound stream; channel.send results are routed to their waiting caller by
// ID. The inbound stream is closed when the loop ends.
func (c *ChannelClient) readLoop() {
	defer close(c.done)
	defer close(c.inbound)
	for {
		f, err := c.sess.recv()
		if err != nil {
			c.fail(err)
			return
		}
		switch {
		case f.Type == frameEvent && f.Topic == topicChannelInbound:
			var in ChannelInbound
			if err := json.Unmarshal(f.Payload, &in); err != nil {
				c.log.Warn("channel inbound: bad payload", "channel", c.name, "err", err)
				continue
			}
			select {
			case c.inbound <- in:
			case <-c.done:
				return
			}
		case f.Type == frameResult && f.Topic == topicChannelSend:
			var res ChannelSendResult
			if err := json.Unmarshal(f.Payload, &res); err != nil {
				c.log.Warn("channel send result: bad payload", "channel", c.name, "id", f.ID, "err", err)
				continue
			}
			c.deliver(f.ID, res)
		case f.Type == frameControl && f.Topic == topicHeartbeat:
			// Plugin answered a heartbeat; nothing to do.
		default:
			c.log.Debug("channel: ignoring frame", "channel", c.name, "type", f.Type, "topic", f.Topic)
		}
	}
}

// deliver routes a send result to its waiter; a missing id is dropped, not fatal.
func (c *ChannelClient) deliver(id uint64, res ChannelSendResult) {
	c.mu.Lock()
	ch, ok := c.pending[id]
	c.mu.Unlock()
	if ok {
		ch <- res
	}
}

// fail records why the loop stopped and marks the client closed.
func (c *ChannelClient) fail(err error) {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.loopErr = err
	}
	c.mu.Unlock()
}

// drainStderr forwards the plugin's stderr lines into the host logger.
func (c *ChannelClient) drainStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		c.log.Info("channel stderr", "channel", c.name, "line", sc.Text())
	}
}

// Close asks the plugin to shut down gracefully, then waits for the process to exit,
// killing it if it lingers.
func (c *ChannelClient) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()

	_ = c.sess.send(frame{Type: frameControl, Topic: topicShutdown})
	return c.shutdown()
}
