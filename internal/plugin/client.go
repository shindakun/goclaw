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

// Client drives one running plugin process. It is the host-side counterpart to the
// SDK's Serve: it launches the binary, runs the handshake, and turns Invoke calls
// into tool.invoke request frames whose results it correlates back by frame ID.
//
// The SDK dispatches invokes concurrently and may reply out of order, so Client
// runs a single read loop and routes each result to the waiting caller by ID.
type Client struct {
	name string
	cmd  *exec.Cmd
	sess *session
	log  *slog.Logger

	info Info // filled by the handshake

	nextID atomic.Uint64

	mu      sync.Mutex
	pending map[uint64]chan result // id -> waiter
	closed  bool
	loopErr error // why the read loop stopped (nil until it does)
	done    chan struct{}
}

// Launch starts the plugin binary at execPath, wires its stdio, performs the
// handshake, and returns a ready Client. env is the process environment (the host
// supplies credential VALUES here; see the design doc). The returned Client runs a
// background read loop until Close or the process exits.
func Launch(ctx context.Context, name, execPath string, env []string, log *slog.Logger) (*Client, error) {
	cmd := exec.CommandContext(ctx, execPath)
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin %q: stdin pipe: %w", name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin %q: stdout pipe: %w", name, err)
	}
	// The plugin logs to stderr; fan it into the host logger via a scanner.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin %q: stderr pipe: %w", name, err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("plugin %q: start %s: %w", name, execPath, err)
	}

	c := &Client{
		name:    name,
		cmd:     cmd,
		sess:    newSession(stdout, stdin),
		log:     log,
		pending: make(map[uint64]chan result),
		done:    make(chan struct{}),
	}
	go c.drainStderr(stderr)

	if err := c.handshake(); err != nil {
		_ = c.kill()
		return nil, err
	}
	go c.readLoop()
	return c, nil
}

// Info returns the plugin's announced identity (kind, version, advertised tools).
func (c *Client) Info() Info { return c.info }

// handshake sends hello and waits for a matching hello.ok, recording the plugin's
// Info. It runs before the read loop starts, so it reads the reply directly.
func (c *Client) handshake() error {
	payload, _ := json.Marshal(hello{Magic: magic, ProtocolVer: protocolVer})
	if err := c.sess.send(frame{Type: frameControl, Topic: topicHello, Payload: payload}); err != nil {
		return fmt.Errorf("plugin %q: send hello: %w", c.name, err)
	}
	f, err := c.sess.recv()
	if err != nil {
		return fmt.Errorf("plugin %q: read hello.ok: %w", c.name, err)
	}
	if f.Type != frameControl || f.Topic != topicHelloOK {
		return fmt.Errorf("plugin %q: expected hello.ok, got type=%d topic=%q", c.name, f.Type, f.Topic)
	}
	var hok helloOK
	if err := json.Unmarshal(f.Payload, &hok); err != nil {
		return fmt.Errorf("plugin %q: bad hello.ok payload: %w", c.name, err)
	}
	if hok.Magic != magic || hok.ProtocolVer != protocolVer {
		return fmt.Errorf("plugin %q: handshake mismatch: plugin magic=%q ver=%d, host magic=%q ver=%d",
			c.name, hok.Magic, hok.ProtocolVer, magic, protocolVer)
	}
	c.info = hok.Info
	return nil
}

// Invoke calls a tool by name with raw JSON args and returns its result text. A
// plugin-reported tool error is returned as a non-nil error (so callers see the
// failure), with the result text as the message.
func (c *Client) Invoke(ctx context.Context, tool string, args json.RawMessage) (string, error) {
	id := c.nextID.Add(1)
	ch := make(chan result, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return "", fmt.Errorf("plugin %q: closed (%v)", c.name, c.loopErr)
	}
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	payload, err := json.Marshal(invoke{Tool: tool, Args: args})
	if err != nil {
		return "", fmt.Errorf("plugin %q: marshal invoke: %w", c.name, err)
	}
	if err := c.sess.send(frame{Type: frameRequest, ID: id, Topic: topicInvoke, Payload: payload}); err != nil {
		return "", fmt.Errorf("plugin %q: send invoke: %w", c.name, err)
	}

	select {
	case res := <-ch:
		if res.IsError {
			return res.Text, fmt.Errorf("plugin %q tool %q: %s", c.name, tool, res.Text)
		}
		return res.Text, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-c.done:
		return "", fmt.Errorf("plugin %q: process ended before result (%v)", c.name, c.loopErr)
	}
}

// readLoop reads frames until the transport closes, routing each tool.invoke result
// to its waiting Invoke caller by ID.
func (c *Client) readLoop() {
	defer close(c.done)
	for {
		f, err := c.sess.recv()
		if err != nil {
			c.fail(err)
			return
		}
		switch {
		case f.Type == frameResult && f.Topic == topicInvoke:
			var res result
			if err := json.Unmarshal(f.Payload, &res); err != nil {
				c.log.Warn("plugin result: bad payload", "plugin", c.name, "id", f.ID, "err", err)
				continue
			}
			c.deliver(f.ID, res)
		case f.Type == frameControl && f.Topic == topicHeartbeat:
			// Plugin answered a heartbeat we sent; nothing to do for now.
		default:
			c.log.Debug("plugin: ignoring frame", "plugin", c.name, "type", f.Type, "topic", f.Topic)
		}
	}
}

// deliver routes a result to its waiter; a missing id (timed-out or unknown) is
// dropped, not fatal.
func (c *Client) deliver(id uint64, res result) {
	c.mu.Lock()
	ch, ok := c.pending[id]
	c.mu.Unlock()
	if ok {
		ch <- res
	}
}

// fail records why the loop stopped and unblocks every pending caller (via done).
func (c *Client) fail(err error) {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.loopErr = err
	}
	c.mu.Unlock()
}

// drainStderr forwards the plugin's stderr lines into the host logger.
func (c *Client) drainStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		c.log.Info("plugin stderr", "plugin", c.name, "line", sc.Text())
	}
}

// Close asks the plugin to shut down gracefully (a shutdown control frame), then
// waits for the process to exit, killing it if it lingers.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()

	_ = c.sess.send(frame{Type: frameControl, Topic: topicShutdown})
	return c.kill()
}

// kill terminates the process and reaps it.
func (c *Client) kill() error {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	return c.cmd.Wait()
}
