package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shindakun/goclaw/internal/plugin"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// installRoll builds the roll demo plugin from the sibling goclawkit module into a
// temp plugins/ dir (binary + plugin.yml), or skips if goclawkit is absent.
func installRoll(t *testing.T) string {
	t.Helper()
	kit := "../../../goclawkit/cmd/roll"
	if _, err := os.Stat(kit); err != nil {
		t.Skipf("goclawkit not found at %s; skipping live plugin test", kit)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "roll")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "roll"), "./cmd/roll")
	cmd.Dir = "../../../goclawkit"
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build roll: %v\n%s", err, b)
	}
	yml := "name: roll\nkind: tool\nversion: \"1.0.0\"\nexec: roll\n" +
		"description: Roll dice in NdM notation.\ncommand: roll\nenv: []\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLoadPlugins_ToolAndCommand(t *testing.T) {
	root := installRoll(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ph := loadPlugins(ctx, root, quietLog())
	if ph == nil {
		t.Fatal("expected a plugin host with the roll plugin loaded")
	}
	defer ph.Close()

	// The MCP server exposes the roll tool to the agent.
	if ph.server == nil {
		t.Fatal("expected an MCP server")
	}

	// The slash command dispatches directly to the plugin (no agent turn).
	out, ok := ph.command(ctx, "/roll 2d6")
	if !ok {
		t.Fatal("/roll should be handled by a plugin")
	}
	if !strings.HasPrefix(out, "2d6 -> [") {
		t.Fatalf("unexpected /roll output: %q", out)
	}
}

// An unrelated slash command is not claimed by any plugin (falls through to agent).
func TestPluginHost_UnknownCommandFallsThrough(t *testing.T) {
	root := installRoll(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ph := loadPlugins(ctx, root, quietLog())
	if ph == nil {
		t.Fatal("plugin host nil")
	}
	defer ph.Close()

	if _, ok := ph.command(ctx, "/nope"); ok {
		t.Fatal("/nope should not be claimed by any plugin")
	}
	// Plain text is never a command.
	if _, ok := ph.command(ctx, "hello there"); ok {
		t.Fatal("plain text should not be a command")
	}
}

// A nil pluginHost (no plugins installed) is a safe no-op for command dispatch.
func TestPluginHost_NilSafe(t *testing.T) {
	var ph *pluginHost
	if _, ok := ph.command(context.Background(), "/roll 2d6"); ok {
		t.Fatal("nil pluginHost must not claim a command")
	}
	ph.Close() // must not panic
}

// argsForTool maps a single-string-schema tool from a raw arg line.
func TestArgsForTool_SingleString(t *testing.T) {
	tool := plugin.ToolInfo{
		Name:        "roll",
		InputSchema: []byte(`{"type":"object","properties":{"notation":{"type":"string"}}}`),
	}
	got, err := argsForTool(tool, "2d6")
	if err != nil {
		t.Fatalf("argsForTool: %v", err)
	}
	if string(got) != `{"notation":"2d6"}` {
		t.Fatalf("got %s", got)
	}
}
