package vaultinit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstall_FreshDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	res, err := Install(dir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(res.Written) == 0 {
		t.Fatal("expected files written into a fresh vault")
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("fresh install should skip nothing, skipped %v", res.Skipped)
	}
	// The template ships a CLAUDE.md system prompt; it must land.
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatalf("CLAUDE.md not installed: %v", err)
	}
	// Result.Dir is absolute.
	if !filepath.IsAbs(res.Dir) {
		t.Fatalf("Result.Dir not absolute: %q", res.Dir)
	}
}

// Install is idempotent: a second run over the same dir overwrites nothing and
// reports every file as skipped, writing none.
func TestInstall_Idempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	first, err := Install(dir)
	if err != nil {
		t.Fatalf("first Install: %v", err)
	}
	second, err := Install(dir)
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if len(second.Written) != 0 {
		t.Fatalf("second install wrote files: %v", second.Written)
	}
	if len(second.Skipped) != len(first.Written) {
		t.Fatalf("skipped %d on rerun, want %d (all first-run files)",
			len(second.Skipped), len(first.Written))
	}
}

// An existing file must never be clobbered; its content survives Install.
func TestInstall_DoesNotOverwrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(custom, []byte("MY CUSTOM PROMPT"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Install(dir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	got, err := os.ReadFile(custom)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "MY CUSTOM PROMPT" {
		t.Fatalf("Install overwrote an existing file; content = %q", got)
	}
	// And it was reported as skipped, not written.
	var skipped bool
	for _, s := range res.Skipped {
		if s == "CLAUDE.md" {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("CLAUDE.md not in Skipped: %v", res.Skipped)
	}
}

// On a fresh non-repo dir, Install runs git init (when git is present).
func TestInstall_GitInit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	res, err := Install(dir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.GitInited {
		// git was available and initialized: a .git dir must exist.
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			t.Fatalf("GitInited true but no .git: %v", err)
		}
	} else {
		t.Log("git not available in this environment; GitInited=false is acceptable")
	}
}

// Installing into an existing git repo must not re-init it.
func TestInstall_ExistingRepoNotReinited(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Install(dir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.GitInited {
		t.Fatal("GitInited should be false when dir is already a repo")
	}
}
