package main

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/shindakun/goclaw/internal/plugin"
)

// The runner launches a kind:channel plugin in the sandbox and bridges its stdio to the
// host relay, dialing whatever the host's .endpoint file in the plugin dir says (a Unix
// socket on native Linux, or TCP to host.docker.internal across the macOS podman VM). The
// host never runs the plugin: it runs here, in the sandbox, and the host only connects.

// loadedChannel is one running channel plugin: its process, the relay connection
// bridging it to the host, and the machinery to tear both down.
type loadedChannel struct {
	name string
	dir  string
	cmd  *exec.Cmd
	conn net.Conn

	stopOnce sync.Once
	done     chan struct{} // closed to signal the pump goroutines to stop
}

// launchChannel starts a kind=channel plugin in the sandbox and bridges its stdio to
// the host over the per-channel Unix socket. Unlike a tool plugin (an in-process MCP
// server), a channel is long-lived and bidirectional: the host drives the channel.*
// protocol over the socket, the runner just moves bytes. Errors are logged, not fatal.
func (ph *pluginHost) launchChannel(ctx context.Context, man plugin.Manifest, pdir string) {
	// Only the manifest's allowlisted env names cross to the plugin, on a PATH-only
	// base, never the runner's full environment (same rule as tool plugins).
	env := man.InjectEnv(plugin.MinimalEnvBase(), os.LookupEnv)

	cmd := exec.CommandContext(ctx, man.ExecPath())
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		ph.log.Error("channel: stdin pipe", "plugin", man.Name, "err", err)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		ph.log.Error("channel: stdout pipe", "plugin", man.Name, "err", err)
		return
	}
	cmd.Stderr = os.Stderr // the plugin's logs fan into the runner's stderr
	if err := cmd.Start(); err != nil {
		ph.log.Error("channel: start", "plugin", man.Name, "err", err)
		return
	}

	// Read the host's dial info (.endpoint) from the plugin dir. The host writes it when
	// it opens the relay; it tells us whether to dial a Unix socket or a TCP address (and
	// carries the TCP auth token). Retry the read briefly: the host may write .endpoint
	// slightly after the plugin dir appears.
	ep, err := readEndpoint(ctx, pdir, 10*time.Second)
	if err != nil {
		ph.log.Error("channel: read endpoint", "plugin", man.Name, "dir", pdir, "err", err)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return
	}
	// Dial the host relay per the endpoint (unix or tcp), retrying until it accepts.
	conn, err := plugin.DialChannelEndpoint(ep, 10*time.Second)
	if err != nil {
		ph.log.Error("channel: dial host relay", "plugin", man.Name, "endpoint", ep.LogString(), "err", err)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return
	}

	lc := &loadedChannel{
		name: man.Name,
		dir:  pdir,
		cmd:  cmd,
		conn: conn,
		done: make(chan struct{}),
	}

	// Bridge: relay conn -> plugin stdin, plugin stdout -> relay conn. Either direction
	// ending (process exit or host disconnect) tears the channel down.
	go lc.pump(ph.log, conn, stdin, "host->plugin")
	go lc.pump(ph.log, stdout, conn, "plugin->host")

	ph.mu.Lock()
	ph.channels[man.Name] = lc
	ph.mu.Unlock()
	ph.log.Info("channel plugin launched", "plugin", man.Name, "endpoint", ep.LogString())
}

// readEndpoint reads the host-written .endpoint from the plugin dir, retrying until it
// appears or the deadline passes.
func readEndpoint(ctx context.Context, pdir string, timeout time.Duration) (plugin.ChannelEndpoint, error) {
	deadline := time.Now().Add(timeout)
	for {
		ep, err := plugin.ReadChannelEndpoint(pdir)
		if err == nil {
			return ep, nil
		}
		if ctx.Err() != nil {
			return plugin.ChannelEndpoint{}, ctx.Err()
		}
		if time.Now().After(deadline) {
			return plugin.ChannelEndpoint{}, err
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return plugin.ChannelEndpoint{}, ctx.Err()
		}
	}
}

// pump copies src->dst until one side closes, then tears the channel down so the other
// direction unwinds too. The label is for logging.
func (lc *loadedChannel) pump(log loggerLike, src io.Reader, dst io.Writer, label string) {
	_, err := io.Copy(dst, src)
	select {
	case <-lc.done:
		// Already stopping; the error (if any) is expected (closed conn/pipe).
	default:
		if err != nil {
			log.Debug("channel: bridge ended", "plugin", lc.name, "dir", label, "err", err)
		}
		lc.stop()
	}
}

// stop kills the plugin process and closes the socket, once. The pump goroutines see
// the closed conn/pipe and exit; done guards against a pump re-entering stop.
func (lc *loadedChannel) stop() {
	lc.stopOnce.Do(func() {
		close(lc.done)
		if lc.conn != nil {
			_ = lc.conn.Close()
		}
		if lc.cmd != nil && lc.cmd.Process != nil {
			_ = lc.cmd.Process.Kill()
			_ = lc.cmd.Wait()
		}
	})
}

// loggerLike is the slice of *slog.Logger the pump uses (kept tiny so a test can pass a
// stub without constructing a real logger).
type loggerLike interface {
	Debug(msg string, args ...any)
}
