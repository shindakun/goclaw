package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func writeFileSize(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRotateReason_SizeTrigger(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	writeFileSize(t, p, 3*1024*1024)
	cfg := rotateConfig{maxBytes: 2 * 1024 * 1024, maxAge: 0}

	reason := rotateReason(p, cfg, time.Now())
	if reason == "" || !strings.Contains(reason, "cap") {
		t.Fatalf("expected size rotation, got %q", reason)
	}
}

func TestRotateReason_UnderCap_NoRotate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	writeFileSize(t, p, 1024*1024)
	cfg := rotateConfig{maxBytes: 2 * 1024 * 1024, maxAge: 0}

	if reason := rotateReason(p, cfg, time.Now()); reason != "" {
		t.Fatalf("expected no rotation under cap, got %q", reason)
	}
}

func TestRotateReason_AgeTrigger(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	// First line carries an old timestamp; small file so size won't trigger.
	old := time.Now().Add(-20 * 24 * time.Hour).Format(time.RFC3339)
	if err := os.WriteFile(p, []byte(`{"timestamp":"`+old+`","x":1}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := rotateConfig{maxBytes: 100 * 1024 * 1024, maxAge: 14 * 24 * time.Hour}

	reason := rotateReason(p, cfg, time.Now())
	if reason == "" || !strings.Contains(reason, "old") {
		t.Fatalf("expected age rotation, got %q", reason)
	}
}

func TestRotateReason_AgeDisabled(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	old := time.Now().Add(-100 * 24 * time.Hour).Format(time.RFC3339)
	if err := os.WriteFile(p, []byte(`{"timestamp":"`+old+`"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := rotateConfig{maxBytes: 100 * 1024 * 1024, maxAge: 0} // age disabled

	if reason := rotateReason(p, cfg, time.Now()); reason != "" {
		t.Fatalf("age check disabled but rotated: %q", reason)
	}
}

func TestRotateReason_UnparseableTimestamp_SkipsAge(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(p, []byte(`{"no":"timestamp here"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := rotateConfig{maxBytes: 100 * 1024 * 1024, maxAge: 24 * time.Hour}
	// No parseable start time -> age check is skipped, not a false rotation.
	if reason := rotateReason(p, cfg, time.Now()); reason != "" {
		t.Fatalf("unparseable timestamp should skip age check, got %q", reason)
	}
}

func TestLoadRotateConfig_Defaults(t *testing.T) {
	os.Unsetenv("GOCLAW_TRANSCRIPT_ROTATE_BYTES")
	os.Unsetenv("GOCLAW_TRANSCRIPT_ROTATE_AGE_DAYS")
	cfg := loadRotateConfig()
	if cfg.maxBytes != defaultRotateBytes {
		t.Fatalf("default bytes = %d, want %d", cfg.maxBytes, defaultRotateBytes)
	}
	if cfg.maxAge != defaultRotateAgeDays*24*time.Hour {
		t.Fatalf("default age = %v", cfg.maxAge)
	}
}

func TestLoadRotateConfig_Overrides(t *testing.T) {
	t.Setenv("GOCLAW_TRANSCRIPT_ROTATE_BYTES", "5000000")
	t.Setenv("GOCLAW_TRANSCRIPT_ROTATE_AGE_DAYS", "3")
	cfg := loadRotateConfig()
	if cfg.maxBytes != 5_000_000 {
		t.Fatalf("override bytes = %d", cfg.maxBytes)
	}
	if cfg.maxAge != 3*24*time.Hour {
		t.Fatalf("override age = %v", cfg.maxAge)
	}
}

func TestLoadRotateConfig_AgeZeroDisables(t *testing.T) {
	t.Setenv("GOCLAW_TRANSCRIPT_ROTATE_AGE_DAYS", "0")
	cfg := loadRotateConfig()
	if cfg.maxAge != 0 {
		t.Fatalf("age 0 should disable, got %v", cfg.maxAge)
	}
}

// TestMaybeRotate_MovesAsideAndReports exercises the end-to-end guard: a fat
// transcript under the expected projects path is moved aside and a reason
// returned, so the caller drops the session id.
func TestMaybeRotate_MovesAsideAndReports(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "") // use HOME/.claude
	projects := filepath.Join(home, ".claude", "projects", "-work")
	if err := os.MkdirAll(projects, 0o777); err != nil {
		t.Fatal(err)
	}
	sid := "11111111-2222-3333-4444-555555555555"
	tp := filepath.Join(projects, sid+".jsonl")
	writeFileSize(t, tp, 20*1024*1024)

	r := &runner{rotate: rotateConfig{maxBytes: 12 * 1024 * 1024}, log: quietLogger()}
	reason := r.maybeRotateTranscript(sid)
	if reason == "" {
		t.Fatal("expected rotation of an oversized transcript")
	}
	if _, err := os.Stat(tp); !os.IsNotExist(err) {
		t.Fatalf("transcript should have been moved aside; stat err = %v", err)
	}
	// The aside copy should exist (preserved, not destroyed).
	matches, _ := filepath.Glob(tp + ".rotated-*")
	if len(matches) != 1 {
		t.Fatalf("expected one rotated-aside file, found %d", len(matches))
	}
}
