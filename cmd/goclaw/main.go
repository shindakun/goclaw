// Command goclaw is the host orchestrator entry point. It loads config,
// initializes the central DB, registers channel adapters, and starts the
// router, delivery, and sweep loops under a single cancellable context. A
// SIGINT/SIGTERM cancels that context and every loop unwinds for graceful
// shutdown (brief §5.1, §10).
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sync/errgroup"

	"github.com/shindakun/goclaw/internal/channels"
	"github.com/shindakun/goclaw/internal/channels/telegram"
	"github.com/shindakun/goclaw/internal/config"
	"github.com/shindakun/goclaw/internal/db"
	"github.com/shindakun/goclaw/internal/delivery"
	"github.com/shindakun/goclaw/internal/router"
	"github.com/shindakun/goclaw/internal/sweep"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
	log.Info("shutdown complete")
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Root context cancelled on SIGINT/SIGTERM (brief §5.1, graceful shutdown).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Central DB (creates data dir + schema on first run).
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	central, err := db.Open(cfg.CentralDBPath)
	if err != nil {
		return err
	}
	defer central.Close()
	log.Info("central db ready", "path", cfg.CentralDBPath)

	// Optional startup seeding so a first user can message the host without
	// hand-editing the DB (brief §3.4). Idempotent.
	_, agentGroupID, err := central.Apply(db.Bootstrap{
		OwnerTelegramID:         cfg.OwnerTelegramID,
		DefaultAgentGroupName:   cfg.DefaultAgentGroupName,
		DefaultAgentGroupFolder: cfg.DefaultAgentGroupFolder,
	})
	if err != nil {
		return err
	}
	if cfg.OwnerTelegramID != "" {
		log.Info("seeded owner", "telegram_id", cfg.OwnerTelegramID)
	}

	// Owner auto-wiring is opt-in and only meaningful with a default agent group.
	var autoWireID int64
	if cfg.AutoWireOwner {
		autoWireID = agentGroupID
		log.Info("owner auto-wire enabled", "agent_group", autoWireID)
	}

	// Channel registry. Telegram is the v0 channel (brief §7.4); register it
	// only when a token is configured.
	registry := channels.NewRegistry()
	if cfg.TelegramToken != "" {
		tg, err := telegram.New(cfg.TelegramToken)
		if err != nil {
			return err
		}
		if err := registry.Register(tg); err != nil {
			return err
		}
		log.Info("registered channel", "channel", tg.Name())
	} else {
		log.Warn("TELEGRAM_BOT_TOKEN unset — no channels registered")
	}

	// Start adapters and fan inbound messages in.
	inbound, err := registry.StartAll(ctx)
	if err != nil {
		return err
	}

	// Wire the host loops. errgroup ties their lifetimes to ctx: if any returns
	// a non-nil error, the group cancels and the rest unwind.
	rtr := router.New(central, cfg.DataDir, autoWireID, log)
	del := delivery.New(central, registry, cfg.DataDir, log)
	swp := sweep.New(central, log)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return rtr.Run(gctx, inbound) })
	g.Go(func() error { return del.Run(gctx) })
	g.Go(func() error { return swp.Run(gctx) })

	log.Info("goclaw host started")
	err = g.Wait()
	if errors.Is(err, context.Canceled) {
		return nil // clean shutdown
	}
	return err
}
