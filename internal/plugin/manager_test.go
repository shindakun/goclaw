package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shindakun/goclaw/internal/command"
)

// fakeSink captures command registrations so a test can invoke them.
type fakeSink struct {
	mu  sync.Mutex
	cmd map[string]command.Command
}

func newFakeSink() *fakeSink { return &fakeSink{cmd: map[string]command.Command{}} }

func (f *fakeSink) Register(c command.Command) {
	f.mu.Lock()
	f.cmd[c.Name] = c
	f.mu.Unlock()
}

func (f *fakeSink) UnregisterSource(source string) {
	f.mu.Lock()
	for name, c := range f.cmd {
		if c.Source == source {
			delete(f.cmd, name)
		}
	}
	f.mu.Unlock()
}

func (f *fakeSink) get(name string) (command.Command, bool) {
	f.mu.Lock()
	c, ok := f.cmd[name]
	f.mu.Unlock()
	return c, ok
}

// installRoll builds the roll plugin into a temp plugins/ dir with its plugin.yml,
// or skips if goclawkit is not present.
func installRoll(t *testing.T) string {
	t.Helper()
	bin := rollBin(t) // builds from sibling goclawkit, or skips
	root := t.TempDir()
	dir := filepath.Join(root, "roll")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Copy the built binary in.
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "roll"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	// A minimal manifest matching the roll plugin.
	yml := "name: roll\nkind: tool\nversion: \"1.0.0\"\nexec: roll\n" +
		"description: Roll dice in NdM notation.\ncommand: roll\nenv: []\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestManager_LoadAndCommand(t *testing.T) {
	root := installRoll(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sink := newFakeSink()
	m := NewManager(root, sink, os.Environ(), quietLog())
	if err := m.LoadAll(ctx, nil /* all enabled */); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	defer m.Close()

	if names := m.Names(); len(names) != 1 || names[0] != "roll" {
		t.Fatalf("expected roll loaded, got %v", names)
	}

	// The /roll command must be registered, and invoking it with "2d6" runs the
	// tool through the live plugin.
	cmd, ok := sink.get("roll")
	if !ok {
		t.Fatal("/roll command not registered")
	}
	out, err := cmd.Handler(ctx, command.Request{Args: "2d6"})
	if err != nil {
		t.Fatalf("command handler: %v", err)
	}
	if !strings.HasPrefix(out, "2d6 -> [") {
		t.Fatalf("unexpected /roll output: %q", out)
	}
}

// A disabled plugin is discovered but not launched or registered.
func TestManager_DisabledSkipped(t *testing.T) {
	root := installRoll(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sink := newFakeSink()
	m := NewManager(root, sink, os.Environ(), quietLog())
	if err := m.LoadAll(ctx, func(name string) bool { return false }); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	defer m.Close()

	if names := m.Names(); len(names) != 0 {
		t.Fatalf("disabled plugin should not be loaded, got %v", names)
	}
	if _, ok := sink.get("roll"); ok {
		t.Fatal("disabled plugin should not register a command")
	}
}

// A missing plugins dir is not an error.
func TestManager_MissingDir(t *testing.T) {
	m := NewManager(filepath.Join(t.TempDir(), "nope"), newFakeSink(), nil, quietLog())
	if err := m.LoadAll(context.Background(), nil); err != nil {
		t.Fatalf("missing dir should be a no-op, got %v", err)
	}
}
