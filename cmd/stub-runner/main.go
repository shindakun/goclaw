// Command stub-runner is a minimal stand-in for the in-container agent-runner.
// It does NOT run Claude — it just proves the SQLite boundary end-to-end: poll
// inbound.db for pending messages, mark them consumed, and write an echo reply
// into outbound.db for the host's delivery loop to pick up (brief §3.1, Phase 0).
//
// It operates on a session directory containing inbound.db + outbound.db. Inside
// a real container that directory is the mount; for local end-to-end testing you
// point it at a session dir under data/v2-sessions/.../.
//
// Usage:
//
//	stub-runner -dir data/v2-sessions/1/telegram_555
//	stub-runner -dir <dir> -once     # process current backlog and exit
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shindakun/goclaw/internal/db"
)

func main() {
	dir := flag.String("dir", "", "session directory containing inbound.db and outbound.db")
	once := flag.Bool("once", false, "process the current backlog once and exit")
	interval := flag.Duration("interval", 500*time.Millisecond, "poll interval")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if *dir == "" {
		log.Error("missing -dir")
		os.Exit(2)
	}
	if err := run(*dir, *once, *interval, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(dir string, once bool, interval time.Duration, log *slog.Logger) error {
	log.Info("stub-runner started", "dir", dir)

	if once {
		n, err := processOnce(dir, log)
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
			if _, err := processOnce(dir, log); err != nil {
				log.Error("process", "err", err)
			}
		}
	}
}

// processOnce opens the session FRESH, handles every currently-pending inbound
// message, and closes again. Opening per poll is deliberate: the session DBs
// live on a container bind mount, and a long-lived SQLite handle caches the
// file's pages and never sees the host's later writes across the VM's shared
// filesystem. A fresh open each poll always reads current on-disk state.
func processOnce(dir string, log *slog.Logger) (int, error) {
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
		if err := sess.MarkInboundConsumed(m.ID); err != nil {
			return 0, err
		}
		log.Info("echoed", "in_id", m.ID, "text", m.Text)
	}
	return len(pending), nil
}
