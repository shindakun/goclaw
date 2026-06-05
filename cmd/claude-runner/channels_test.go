package main

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/shindakun/goclaw/internal/plugin"
)

// TestLaunchChannel_BridgesStdioToEndpoint proves the runner's channel glue reads the
// host-written .endpoint, dials it, and connects the plugin process's stdin/stdout to
// that connection in BOTH directions. It uses `cat` as a stand-in plugin (echoes stdin
// to stdout) and a unix endpoint (the test runs natively, so the endpoint path is a real
// socket): a line the host writes should come back, having passed
// conn -> plugin stdin -> (cat) -> plugin stdout -> conn.
func TestLaunchChannel_BridgesStdioToEndpoint(t *testing.T) {
	// Host side: listen on a unix socket the runner will dial via the .endpoint.
	// The socket path must stay under the OS sun_path limit (104 on macOS, 108 on
	// Linux); t.TempDir() bakes the long test name into the path and overflows it, so
	// bind the socket in a SHORT dir directly under the OS temp root instead.
	sockDir, err := os.MkdirTemp("", "chsock")
	if err != nil {
		t.Fatalf("sock dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(sockDir) }()
	sockPath := filepath.Join(sockDir, "e.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("host listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	hostConn := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			hostConn <- c
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ph := &pluginHost{
		log:      quietLog(),
		loaded:   map[string]*loadedPlugin{},
		channels: map[string]*loadedChannel{},
		cmds:     map[string]boundCommand{},
	}

	// A manifest whose exec is `cat` (resolved on PATH via a symlink in a plugin dir).
	pdir := t.TempDir()
	catPath, err := exec.LookPath("cat")
	if err != nil {
		t.Skipf("cat not found: %v", err)
	}
	if err := os.Symlink(catPath, filepath.Join(pdir, "echo")); err != nil {
		t.Fatalf("symlink cat: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "plugin.yml"),
		[]byte("name: echo\nkind: channel\nexec: echo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	man, err := plugin.LoadManifest(pdir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}

	// The host writes .endpoint into the plugin dir; the runner reads it to know how to
	// dial. Here it points at the real test socket (native run, so the path is direct).
	if err := plugin.WriteChannelEndpoint(pdir, plugin.ChannelEndpoint{Transport: "unix", Path: sockPath}); err != nil {
		t.Fatalf("write endpoint: %v", err)
	}

	ph.launchChannel(ctx, man, pdir)

	// The runner should have registered the channel and dialed the socket.
	var conn net.Conn
	select {
	case conn = <-hostConn:
	case <-time.After(5 * time.Second):
		t.Fatal("runner never dialed the host socket")
	}
	defer func() { _ = conn.Close() }()

	ph.mu.Lock()
	_, ok := ph.channels["echo"]
	ph.mu.Unlock()
	if !ok {
		t.Fatal("channel not registered in pluginHost")
	}

	// Write a line to the socket; it should echo back through the plugin's stdio.
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatalf("write to socket: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read echoed line: %v", err)
	}
	if got != "ping\n" {
		t.Fatalf("echoed %q, want \"ping\\n\"", got)
	}

	// Cleanup: dropping the channel (dir gone) stops the process and closes the conn.
	ph.dropMissing(map[string]bool{})
	ph.mu.Lock()
	_, stillThere := ph.channels["echo"]
	ph.mu.Unlock()
	if stillThere {
		t.Fatal("channel not removed after dropMissing")
	}
}
