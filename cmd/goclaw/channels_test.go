package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/shindakun/goclaw/internal/channels"
	chanplugin "github.com/shindakun/goclaw/internal/channels/plugin"
	"github.com/shindakun/goclaw/internal/plugin"
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

	writePlugin(t, pluginsDir, "irc", "name: irc\nkind: channel\nexec: irc\nenv:\n  - IRC_SERVER\n")
	writePlugin(t, pluginsDir, "roll", "name: roll\nkind: tool\nexec: roll\ncommand: roll\n")
	writePlugin(t, pluginsDir, ".half.installing", "name: x\nkind: channel\nexec: x\n")

	cfg := chanplugin.Config{Transport: chanplugin.TransportTCP, TCPHost: "127.0.0.1", TCPBind: "127.0.0.1"}
	registry := channels.NewRegistry()
	relay, err := setupChannelPlugins(registry, cfg, pluginsDir, quietLog())
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

	// The relay wrote a .endpoint into the channel's plugin dir (so the runner can dial).
	ep, err := plugin.ReadChannelEndpoint(filepath.Join(pluginsDir, "irc"))
	if err != nil {
		t.Fatalf("irc .endpoint not written: %v", err)
	}
	if ep.Transport != "tcp" || ep.Addr == "" || ep.Token == "" {
		t.Fatalf("endpoint = %+v, want tcp with addr+token", ep)
	}
}

// An absent plugins dir is fine (no channels), and still returns a usable relay.
func TestSetupChannelPlugins_NoPluginsDir(t *testing.T) {
	cfg := chanplugin.Config{Transport: chanplugin.TransportTCP, TCPHost: "127.0.0.1", TCPBind: "127.0.0.1"}
	registry := channels.NewRegistry()
	relay, err := setupChannelPlugins(registry, cfg, filepath.Join(t.TempDir(), "absent"), quietLog())
	if err != nil {
		t.Fatalf("setup with absent plugins dir: %v", err)
	}
	defer relay.CloseAll()
	if len(registry.All()) != 0 {
		t.Fatal("registered a channel from an absent plugins dir")
	}
}
