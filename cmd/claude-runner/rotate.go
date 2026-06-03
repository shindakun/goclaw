package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Transcript rotation guard (mirrors NanoClaw's maybeRotateContinuation).
//
// Multi-turn resume reloads the session's on-disk .jsonl transcript in full on
// every turn, and that file grows without bound: auto-compact shrinks the
// CONTEXT (token) window but not the file - each turn still appends. Past a size
// or age threshold a cold container can't reload the .jsonl before the host's
// idle ceiling fires, so it is killed before it can ever reply and then loops
// forever. The guard caps that: before resuming, if the transcript is too big or
// too old, it moves the heavy file aside (preserving it on disk) and drops the
// stored session id so the next turn starts a fresh, small session.
//
// (Phase B will render a readable summary into conversations/ before rotating;
// for now the raw .jsonl is preserved by the rename so nothing is destroyed.)

const (
	// defaultRotateBytes caps the resume transcript. Matches NanoClaw's 12MB
	// default: past this a cold resume risks exceeding the host idle ceiling.
	defaultRotateBytes int64 = 12 * 1024 * 1024
	// defaultRotateAgeDays is the secondary age trigger, measured from the
	// transcript's first entry. Matches NanoClaw's 14 days. 0 disables the age
	// check (size alone governs).
	defaultRotateAgeDays = 14
)

// rotateConfig holds the resolved thresholds. Built once from the environment.
type rotateConfig struct {
	maxBytes int64
	maxAge   time.Duration // 0 means the age check is disabled
}

// loadRotateConfig reads GOCLAW_TRANSCRIPT_ROTATE_BYTES and
// GOCLAW_TRANSCRIPT_ROTATE_AGE_DAYS, falling back to the defaults. An age of 0
// (or negative) disables the age trigger so size alone governs.
func loadRotateConfig() rotateConfig {
	cfg := rotateConfig{
		maxBytes: defaultRotateBytes,
		maxAge:   time.Duration(defaultRotateAgeDays) * 24 * time.Hour,
	}
	if v := os.Getenv("GOCLAW_TRANSCRIPT_ROTATE_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.maxBytes = n
		}
	}
	if v := os.Getenv("GOCLAW_TRANSCRIPT_ROTATE_AGE_DAYS"); v != "" {
		if d, err := strconv.Atoi(v); err == nil {
			if d > 0 {
				cfg.maxAge = time.Duration(d) * 24 * time.Hour
			} else {
				cfg.maxAge = 0 // explicit non-positive disables the age check
			}
		}
	}
	return cfg
}

// claudeProjectsDir is where the claude CLI keeps per-session transcripts, under
// the container's home. Matches HOME=/home/agent in container/claude.Containerfile.
func claudeProjectsDir() string {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/agent"
	}
	if base := os.Getenv("CLAUDE_CONFIG_DIR"); base != "" {
		return filepath.Join(base, "projects")
	}
	return filepath.Join(home, ".claude", "projects")
}

// findTranscriptPath locates the .jsonl backing a session id. The CLI names
// project subdirs by a mangled cwd, so rather than reproduce that we scan the
// project dirs for <sessionID>.jsonl (session ids are UUIDs, so it is
// unambiguous). Returns "" if not found.
func findTranscriptPath(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	projects := claudeProjectsDir()
	entries, err := os.ReadDir(projects)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		candidate := filepath.Join(projects, e.Name(), sessionID+".jsonl")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// transcriptStartTime reads the timestamp of the transcript's first entry, used
// for the age check. Returns the zero time if it can't be read or parsed (the
// caller then skips the age trigger for this file).
func transcriptStartTime(path string) time.Time {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !sc.Scan() {
		return time.Time{}
	}
	var row struct {
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(sc.Bytes(), &row); err != nil || row.Timestamp == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, row.Timestamp)
	if err != nil {
		return time.Time{}
	}
	return t
}

// rotateReason decides whether the transcript at path should be rotated, given
// the config and the current time. Returns a human-readable reason, or "" to
// keep resuming. Pure (now is injected) so it is unit-testable.
func rotateReason(path string, cfg rotateConfig, now time.Time) string {
	info, err := os.Stat(path)
	if err != nil {
		return "" // no transcript to rotate
	}
	if info.Size() > cfg.maxBytes {
		return fmt.Sprintf("transcript %.1fMB > %dMB cap",
			float64(info.Size())/(1024*1024), cfg.maxBytes/(1024*1024))
	}
	if cfg.maxAge > 0 {
		if start := transcriptStartTime(path); !start.IsZero() {
			if age := now.Sub(start); age > cfg.maxAge {
				return fmt.Sprintf("transcript %.1fd old > %.0fd cap",
					age.Hours()/24, cfg.maxAge.Hours()/24)
			}
		}
	}
	return ""
}

// maybeRotateTranscript checks the transcript backing sessionID and, if it is
// too big or too old, moves it aside (preserving it) and returns a non-empty
// reason. The caller then clears the stored session id so the next turn starts
// fresh. Returns "" when nothing needs rotating (the common case).
func (r *runner) maybeRotateTranscript(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	path := findTranscriptPath(sessionID)
	if path == "" {
		return ""
	}
	reason := rotateReason(path, r.rotate, time.Now())
	if reason == "" {
		return ""
	}
	// Preserve the heavy transcript by moving it out of the resume path. (Phase B
	// will first render a summary into conversations/.) A rename failure is not
	// fatal: we still drop the session id so the agent stops trying to resume a
	// transcript too large to load.
	aside := fmt.Sprintf("%s.rotated-%d", path, time.Now().UnixNano())
	if err := os.Rename(path, aside); err != nil {
		r.log.Warn("could not move rotated transcript aside", "path", path, "err", err)
	}
	return reason
}
