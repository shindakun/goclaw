package eventlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readEvents parses every line of the active log into Events. Fails the test on a
// malformed line so a corrupt write is caught, not silently skipped.
func readEvents(t *testing.T, dir string) []Event {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, logName))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("malformed event line %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

func TestEmit_AppendsTypedEventsWithFields(t *testing.T) {
	dir := t.TempDir()
	l, err := New(dir, Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	l.Emit(KindScheduleFired, nil, map[string]any{"task": "inbox", "owner": float64(7)})
	l.Emit(KindDeliveryFailed, Bool(false), map[string]any{"channel": "telegram"})

	evs := readEvents(t, dir)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(evs), evs)
	}
	if evs[0].Kind != KindScheduleFired {
		t.Fatalf("first kind = %q, want %q", evs[0].Kind, KindScheduleFired)
	}
	if evs[0].Ok != nil {
		t.Fatalf("schedule.fired emitted with no ok should omit it, got %v", *evs[0].Ok)
	}
	if evs[0].Fields["task"] != "inbox" {
		t.Fatalf("fields not preserved: %+v", evs[0].Fields)
	}
	// The ok=false split must round-trip as false, not be dropped (a dropped ok would
	// make a failure look like an unqualified event).
	if evs[1].Ok == nil || *evs[1].Ok != false {
		t.Fatalf("delivery.failed ok = %v, want non-nil false", evs[1].Ok)
	}
	// TS must parse as RFC3339 (not empty, not some other format).
	if _, err := time.Parse(time.RFC3339, evs[0].TS); err != nil {
		t.Fatalf("ts %q not RFC3339: %v", evs[0].TS, err)
	}
}

func TestEmit_NilLoggerIsSafe(t *testing.T) {
	var l *Logger
	l.Emit(KindScheduleFired, nil, nil) // must not panic
}

func TestRotate_BySize(t *testing.T) {
	dir := t.TempDir()
	// Tiny cap so a couple of events trip rotation; age disabled.
	l, err := New(dir, Config{MaxBytes: 80, MaxAge: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// First event creates the active file (rotation check sees no file, so no rotate).
	l.Emit(KindScheduleFired, nil, map[string]any{"task": "a"})
	// By now the active file likely exceeds 80 bytes; the NEXT emit should rotate the
	// big file aside and start a fresh active file holding only the new event.
	l.Emit(KindScheduleFired, nil, map[string]any{"task": "b"})
	l.Emit(KindScheduleFired, nil, map[string]any{"task": "c"})

	// The rotated generation must exist (proof a rotation happened, not just appends).
	if _, err := os.Stat(filepath.Join(dir, rotatedName)); err != nil {
		t.Fatalf("expected a rotated file after exceeding the size cap: %v", err)
	}
	// The active file must be SHORTER than the total written (it was truncated by
	// rotation), proving events did not all pile into one file.
	active := readEvents(t, dir)
	if len(active) == 0 {
		t.Fatal("active file empty after rotation; the post-rotation event was lost")
	}
	if len(active) >= 3 {
		t.Fatalf("active file holds %d events; rotation did not move earlier ones aside", len(active))
	}
}

func TestRotate_ByAge(t *testing.T) {
	dir := t.TempDir()
	// Large size cap so ONLY age can trigger rotation.
	l, err := New(dir, Config{MaxBytes: 1 << 20, MaxAge: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 9, 8, 0, 0, 0, time.Local)
	l.now = func() time.Time { return base }
	l.Emit(KindScheduleFired, nil, map[string]any{"task": "old"}) // starts the age window at 08:00

	// Two hours later (> MaxAge): the next emit must rotate the aged file aside.
	l.now = func() time.Time { return base.Add(2 * time.Hour) }
	l.Emit(KindScheduleFired, nil, map[string]any{"task": "new"})

	if _, err := os.Stat(filepath.Join(dir, rotatedName)); err != nil {
		t.Fatalf("expected rotation by age: %v", err)
	}
	active := readEvents(t, dir)
	if len(active) != 1 || active[0].Fields["task"] != "new" {
		t.Fatalf("after age rotation active file should hold only the new event, got %+v", active)
	}
}

func TestNew_DefaultsApplied(t *testing.T) {
	l, err := New(t.TempDir(), Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if l.cfg.MaxBytes != defaultMaxBytes || l.cfg.MaxAge != defaultMaxAge {
		t.Fatalf("defaults not applied: %+v", l.cfg)
	}
}

func TestEmit_OneLinePerEvent(t *testing.T) {
	dir := t.TempDir()
	l, _ := New(dir, Config{}, nil)
	l.Emit(KindProxyCANew, nil, map[string]any{"detail": "minted"})
	b, err := os.ReadFile(filepath.Join(dir, logName))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "\n"); n != 1 {
		t.Fatalf("want exactly one newline-terminated record, got %d", n)
	}
}
