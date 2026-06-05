package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/shindakun/goclaw/internal/channels"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func writePlugin(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSetupChannelPlugins_RegistersChannelsOnly proves the host-side discovery: a
// kind:channel plugin gets a relay socket bound and its adapter registered; a kind:tool
// plugin and a dot-dir are skipped.
func TestSetupChannelPlugins_RegistersChannelsOnly(t *testing.T) {
	pluginsDir := t.TempDir()
	sockDir := t.TempDir()

	writePlugin(t, pluginsDir, "irc", "name: irc\nkind: channel\nexec: irc\nenv:\n  - IRC_SERVER\n")
	writePlugin(t, pluginsDir, "roll", "name: roll\nkind: tool\nexec: roll\ncommand: roll\n")
	writePlugin(t, pluginsDir, ".half.installing", "name: x\nkind: channel\nexec: x\n")

	registry := channels.NewRegistry()
	relay, err := setupChannelPlugins(registry, sockDir, pluginsDir, quietLog())
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer relay.CloseAll()

	// The channel plugin is registered as an adapter; the tool and dot-dir are not.
	if _, ok := registry.Get("irc"); !ok {
		t.Fatal("irc channel plugin not registered")
	}
	if _, ok := registry.Get("roll"); ok {
		t.Fatal("tool plugin wrongly registered as a channel")
	}
	if _, ok := registry.Get("x"); ok {
		t.Fatal("dot-dir plugin wrongly registered")
	}
	if got := len(registry.All()); got != 1 {
		t.Fatalf("registered %d channels, want 1", got)
	}

	// The relay bound the channel's socket under sockDir.
	if _, err := os.Stat(filepath.Join(sockDir, "irc.sock")); err != nil {
		t.Fatalf("irc socket not bound: %v", err)
	}
}

// An absent plugins dir is fine (no channels), and still returns a usable relay.
func TestSetupChannelPlugins_NoPluginsDir(t *testing.T) {
	registry := channels.NewRegistry()
	relay, err := setupChannelPlugins(registry, t.TempDir(), filepath.Join(t.TempDir(), "absent"), quietLog())
	if err != nil {
		t.Fatalf("setup with absent plugins dir: %v", err)
	}
	defer relay.CloseAll()
	if len(registry.All()) != 0 {
		t.Fatal("registered a channel from an absent plugins dir")
	}
}
