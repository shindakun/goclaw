package plugin

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestInstaller_AddLive does a REAL end-to-end install: it builds the goclaw-roll
// repo inside a throwaway container from goclaw-claude:latest and stages the Linux
// binary. Gated: requires podman + the runner image + network. Skips otherwise.
func TestInstaller_AddLive(t *testing.T) {
	if os.Getenv("GOCLAW_LIVE_INSTALL") == "" {
		t.Skip("set GOCLAW_LIVE_INSTALL=1 to run the live container install test")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	dest := "./testdata-live-plugins"
	_ = os.RemoveAll(dest)
	defer func() { _ = os.RemoveAll(dest) }()
	in := NewInstaller(dest, "goclaw-claude:latest", "podman")
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	res, err := in.Add(ctx, "https://github.com/shindakun/goclaw-roll")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if res.Name != "roll" || res.Command != "roll" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Commit == "" {
		t.Fatal("expected a pinned commit")
	}
	// The staged binary must be a Linux ELF.
	b, err := os.ReadFile(res.Dir + "/roll")
	if err != nil {
		t.Fatalf("read staged binary: %v", err)
	}
	if len(b) < 4 || b[0] != 0x7f || string(b[1:4]) != "ELF" {
		t.Fatal("staged binary is not a Linux ELF")
	}
}
