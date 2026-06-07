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

	ph := startPlugins(ctx, root, quietLog())
	defer ph.Close()

	// The roll plugin loaded and its tool is exposed to the agent.
	if !ph.hasServerTools() {
		t.Fatal("expected the roll tool to be registered")
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
	ph := startPlugins(ctx, root, quietLog())
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

// Hot reload: start with no plugins, then install roll into the watched dir and
// confirm the watcher loads it live (no restart). Then remove it and confirm it
// unloads.
func TestPluginHost_HotReload(t *testing.T) {
	// Build roll once (skips if goclawkit absent).
	staged := installRoll(t) // staged/roll/{roll,plugin.yml}
	src := filepath.Join(staged, "roll")

	root := t.TempDir() // empty plugins dir to start
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ph := startPlugins(ctx, root, quietLog())
	defer ph.Close()
	if ph.hasServerTools() {
		t.Fatal("no plugins should be loaded initially")
	}

	// Install: copy roll into the watched dir (atomic-ish: build then rename).
	dst := filepath.Join(root, "roll")
	tmp := dst + ".installing"
	if err := os.CopyFS(tmp, os.DirFS(src)); err != nil {
		t.Fatalf("copy plugin: %v", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// The watcher should load it within a few seconds.
	if !waitFor(5*time.Second, func() bool {
		_, ok := ph.command(ctx, "/roll 2d6")
		return ok
	}) {
		t.Fatal("roll was not hot-loaded after install")
	}

	// Remove it and confirm it unloads.
	if err := os.RemoveAll(dst); err != nil {
		t.Fatal(err)
	}
	if !waitFor(5*time.Second, func() bool {
		_, ok := ph.command(ctx, "/roll 2d6")
		return !ok
	}) {
		t.Fatal("roll was not unloaded after removal")
	}
}

// TestPluginHost_ReconcileStopsRemoved proves a removed plugin is unloaded by a DIRECT
// reconcile call, i.e. WITHOUT relying on an fsnotify event. This is the regression guard
// for the real bug: /plugins is a read-only virtiofs mount and inotify does not see
// host-side removes across the podman-VM boundary, so the periodic poll (which just calls
// reconcile) is what must stop a removed plugin, e.g. an uninstalled IRC bridge that would
// otherwise keep reconnecting. We exercise reconcile directly to simulate the watch being
// blind.
func TestPluginHost_ReconcileStopsRemoved(t *testing.T) {
	staged := installRoll(t)
	src := filepath.Join(staged, "roll")

	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ph := startPlugins(ctx, root, quietLog())
	defer ph.Close()

	// Place the plugin and load it via a direct reconcile (not the watch).
	dst := filepath.Join(root, "roll")
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatalf("copy plugin: %v", err)
	}
	ph.reconcile(ctx)
	if _, ok := ph.command(ctx, "/roll 2d6"); !ok {
		t.Fatal("plugin not loaded after reconcile")
	}

	// Remove the dir and reconcile again: the plugin must be dropped, with NO watch event
	// involved. This is exactly what the poll backstop does.
	if err := os.RemoveAll(dst); err != nil {
		t.Fatal(err)
	}
	ph.reconcile(ctx)
	if _, ok := ph.command(ctx, "/roll 2d6"); ok {
		t.Fatal("plugin still loaded after removal + reconcile (the poll backstop would not stop it)")
	}
}

func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cond()
}
