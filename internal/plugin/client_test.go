package plugin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rollBin builds the roll demo plugin from the sibling goclawkit module into a temp
// path and returns it, or skips if the SDK module is not present next to goclaw.
func rollBin(t *testing.T) string {
	t.Helper()
	// goclaw repo root is two dirs up from internal/plugin.
	kit := "../../../goclawkit/cmd/roll"
	if _, err := os.Stat(kit); err != nil {
		t.Skipf("goclawkit not found at %s; skipping live plugin test", kit)
	}
	out := filepath.Join(t.TempDir(), "roll")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/roll")
	cmd.Dir = "../../../goclawkit"
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build roll: %v\n%s", err, b)
	}
	return out
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestClient_InvokeRoll(t *testing.T) {
	bin := rollBin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := Launch(ctx, "roll", bin, os.Environ(), quietLog())
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Handshake info is populated.
	if c.Info().Name != "roll" || c.Info().Kind != "tool" {
		t.Fatalf("unexpected info: %+v", c.Info())
	}
	if len(c.Info().Tools) == 0 || c.Info().Tools[0].Name != "roll" {
		t.Fatalf("expected a roll tool advertised, got %+v", c.Info().Tools)
	}

	text, err := c.Invoke(ctx, "roll", json.RawMessage(`{"notation":"2d6"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	// roll returns e.g. "2d6 -> [4, 5] = 9".
	if !strings.HasPrefix(text, "2d6 -> [") || !strings.Contains(text, "] = ") {
		t.Fatalf("unexpected roll result: %q", text)
	}
}

// A bad tool name comes back as a plugin error result (non-nil error), not a hang.
func TestClient_UnknownTool(t *testing.T) {
	bin := rollBin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := Launch(ctx, "roll", bin, os.Environ(), quietLog())
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer func() { _ = c.Close() }()

	_, err = c.Invoke(ctx, "nope", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected an error for an unknown tool")
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("expected unknown-tool error, got %v", err)
	}
}

// Bad notation is a tool-level error (IsError result), surfaced as a non-nil error.
func TestClient_ToolError(t *testing.T) {
	bin := rollBin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := Launch(ctx, "roll", bin, os.Environ(), quietLog())
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer func() { _ = c.Close() }()

	if _, err := c.Invoke(ctx, "roll", json.RawMessage(`{"notation":"junk"}`)); err == nil {
		t.Fatal("expected a tool error for junk notation")
	}
}

// Concurrent invokes must each get their own correlated result (the ID-routing
// path); this is the regression guard for out-of-order replies.
func TestClient_ConcurrentInvokes(t *testing.T) {
	bin := rollBin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	c, err := Launch(ctx, "roll", bin, os.Environ(), quietLog())
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer func() { _ = c.Close() }()

	const n = 20
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			text, err := c.Invoke(ctx, "roll", json.RawMessage(`{"notation":"1d20"}`))
			if err != nil {
				errs <- err
				return
			}
			if !strings.HasPrefix(text, "1d20 -> [") {
				errs <- errUnexpected(text)
				return
			}
			errs <- nil
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent invoke %d: %v", i, err)
		}
	}
}

type errUnexpected string

func (e errUnexpected) Error() string { return "unexpected result: " + string(e) }
