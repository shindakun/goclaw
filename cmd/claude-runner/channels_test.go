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

// TestLaunchChannel_BridgesStdioToSocket proves the runner's channel glue connects a
// plugin process's stdin/stdout to the host's per-channel Unix socket in BOTH
// directions. It uses `cat` as a stand-in plugin (echoes stdin to stdout), so the test
// exercises the byte bridge itself without a real plugin binary or the framed protocol:
// a line the host writes to the socket should come back, having passed
// socket -> plugin stdin -> (cat) -> plugin stdout -> socket.
func TestLaunchChannel_BridgesStdioToSocket(t *testing.T) {
	// Point the in-container socket dir at a temp dir for the test.
	tmp := t.TempDir()
	old := channelSocketDir
	channelSocketDir = tmp
	defer func() { channelSocketDir = old }()

	// Host side: listen on the per-channel socket the runner will dial.
	sockPath := filepath.Join(tmp, "echo.sock")
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
