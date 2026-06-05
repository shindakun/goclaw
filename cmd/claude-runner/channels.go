package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/shindakun/goclaw/internal/plugin"
)

// channelSocketDir is the in-container path where the host mounts the per-channel Unix
// sockets (one <name>.sock per channel plugin). The host listens on each socket; the
// runner DIALS it and bridges the plugin's stdio across, so the framed channel.*
// protocol flows between the in-container plugin and the host relay. The host never
// runs the plugin: it runs here, in the sandbox, and the host only connects.
//
// A var (not a const) so a test can point it at a temp dir; production never reassigns.
var channelSocketDir = "/run/goclaw/channels"

// loadedChannel is one running channel plugin: its process, the socket connection
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

	// Dial the host's per-channel socket. The host listens; we retry until it is up
	// (the host may create the socket slightly after the plugin dir appears, the same
	// lazy-launch posture the container already has).
	sock := filepath.Join(channelSocketDir, man.Name+".sock")
	conn, err := dialSocket(ctx, sock, 10*time.Second)
	if err != nil {
		ph.log.Error("channel: dial host socket", "plugin", man.Name, "sock", sock, "err", err)
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

	// Bridge: socket -> plugin stdin, plugin stdout -> socket. Either direction ending
	// (process exit or host disconnect) tears the channel down.
	go lc.pump(ph.log, conn, stdin, "host->plugin")
	go lc.pump(ph.log, stdout, conn, "plugin->host")

	ph.mu.Lock()
	ph.channels[man.Name] = lc
	ph.mu.Unlock()
	ph.log.Info("channel plugin launched", "plugin", man.Name, "sock", sock)
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

// dialSocket dials a Unix socket, retrying until it connects or the deadline passes.
// The host creates the socket; the runner waits for it.
func dialSocket(ctx context.Context, path string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	var d net.Dialer
	for {
		conn, err := d.DialContext(ctx, "unix", path)
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("dial %s: %w", path, err)
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// loggerLike is the slice of *slog.Logger the pump uses (kept tiny so a test can pass a
// stub without constructing a real logger).
type loggerLike interface {
	Debug(msg string, args ...any)
}
