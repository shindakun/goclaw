// Command stub-runner is a DEV/CI tool: a minimal echo runner that exercises the
// SQLite boundary end-to-end without Node, the claude CLI, or an API key. It does
// NOT run Claude. For each session under its sessions directory it reads pending
// inbound messages, writes an echo reply to outbound.db, and advances the
// processed high-water mark in outbound.db (it never writes inbound.db, matching
// the real runner's single-writer protocol). The production runner is
// cmd/claude-runner; this exists only to smoke-test the plumbing (brief §3.1).
//
// One runner serves a whole agent group: -dir points at the group's sessions
// directory (the parent of the per-session subdirs, each holding inbound.db +
// outbound.db).
//
// Usage:
//
//	stub-runner -dir data/sessions/1             # serve agent group 1 locally
//	stub-runner -dir <dir> -once                 # process current backlog and exit
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/shindakun/goclaw/internal/db"
)

func main() {
	dir := flag.String("dir", "/sessions", "agent-group sessions directory (parent of per-session subdirs)")
	once := flag.Bool("once", false, "process the current backlog once and exit")
	interval := flag.Duration("interval", 500*time.Millisecond, "poll interval")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := run(*dir, *once, *interval, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(sessionsDir string, once bool, interval time.Duration, log *slog.Logger) error {
	log.Info("stub-runner started", "sessions_dir", sessionsDir)

	if once {
		n, err := processAll(sessionsDir, log)
		if err != nil {
			return err
		}
		log.Info("done", "echoed", n)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("stub-runner stopped")
			return nil
		case <-ticker.C:
			if _, err := processAll(sessionsDir, log); err != nil {
				log.Error("process", "err", err)
			}
		}
	}
}

// processAll scans the sessions directory for session subdirs and processes
// each. Discovering subdirs every poll means new sessions (new conversations)
// are picked up without restarting the runner. Returns the total echoed.
func processAll(sessionsDir string, log *slog.Logger) (int, error) {
	entries, err := os.ReadDir(sessionsDir)
	if os.IsNotExist(err) {
		return 0, nil // group dir not created yet - nothing to do
	}
	if err != nil {
		return 0, err
	}
	total := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(sessionsDir, e.Name())
		// A session subdir is one that has an inbound.db.
		if _, err := os.Stat(filepath.Join(dir, "inbound.db")); err != nil {
			continue
		}
		n, err := processSession(dir, log)
		if err != nil {
			// One bad session shouldn't stop the others.
			log.Error("process session", "session", e.Name(), "err", err)
			continue
		}
		total += n
	}
	return total, nil
}

// processSession opens one session FRESH, handles every currently-pending
// inbound message, and closes again. Opening per poll is deliberate: the session
// DBs live on a container bind mount, and a long-lived SQLite handle caches the
// file's pages and never sees the host's later writes across the VM's shared
// filesystem. A fresh open each poll always reads current on-disk state.
func processSession(dir string, log *slog.Logger) (int, error) {
	sess, err := db.OpenSessionDir(dir)
	if err != nil {
		return 0, err
	}
	defer sess.Close()

	pending, err := sess.PendingInbound()
	if err != nil {
		return 0, err
	}
	for _, m := range pending {
		reply := fmt.Sprintf("echo: %s", m.Text)
		if _, err := sess.EnqueueOutbound(m.Channel, m.ChatID, reply); err != nil {
			return 0, err
		}
		// Advance the high-water mark in outbound.db; never write inbound.db.
		if err := sess.SetInboundHWM(m.ID); err != nil {
			return 0, err
		}
		log.Info("echoed", "session", filepath.Base(dir), "in_id", m.ID, "text", m.Text)
	}
	return len(pending), nil
}
