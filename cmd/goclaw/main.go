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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/shindakun/goclaw/internal/channels"
	"github.com/shindakun/goclaw/internal/channels/telegram"
	"github.com/shindakun/goclaw/internal/config"
	"github.com/shindakun/goclaw/internal/credproxy"
	"github.com/shindakun/goclaw/internal/credstore"
	"github.com/shindakun/goclaw/internal/db"
	"github.com/shindakun/goclaw/internal/delivery"
	"github.com/shindakun/goclaw/internal/maintenance"
	"github.com/shindakun/goclaw/internal/mounts"
	"github.com/shindakun/goclaw/internal/router"
	"github.com/shindakun/goclaw/internal/runtime"
	"github.com/shindakun/goclaw/internal/sweep"
	"github.com/shindakun/goclaw/internal/typing"
)

// anthropicHost is the credential-store host key for the Anthropic API; its
// presence (with an encryption key) switches the runner onto the credential
// proxy instead of a raw key (brief §8).
const anthropicHost = "api.anthropic.com"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Subcommands. With no subcommand, goclaw runs the host (the default).
	if len(os.Args) > 1 && os.Args[1] == "vault" {
		if err := runVault(os.Args[2:]); err != nil {
			log.Error("vault", "err", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "auth" {
		if err := runAuth(os.Args[2:]); err != nil {
			log.Error("auth", "err", err)
			os.Exit(1)
		}
		return
	}

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
		log.Warn("TELEGRAM_BOT_TOKEN unset - no channels registered")
	}

	// Start adapters and fan inbound messages in.
	inbound, err := registry.StartAll(ctx)
	if err != nil {
		return err
	}

	// Container runner: when enabled, the host launches a Podman runner per
	// agent group on enqueue and reaps idle ones in the sweep. When disabled
	// (default), start a runner out of band (cmd/claude-runner). The interface
	// values stay nil when disabled (avoid the typed-nil-interface trap) so the
	// router/sweep nil checks work.
	var (
		ensurer router.RunnerEnsurer // narrow: ensure only (router)
		runners sweep.RunnerManager  // richer: ensure + list + stop (sweep GC)
		// proxyStore is non-nil when the bundled credential proxy should run (an
		// Anthropic credential is stored + an encryption key is set). The proxy
		// goroutine below uses it; nil means no proxy (raw key path).
		proxyStore *credstore.Store
	)
	if cfg.LaunchRunner {
		// Load the external mount allowlist (fail-closed if absent) to validate
		// any extra group mounts at launch (brief §6.3).
		allow, err := mounts.LoadAllowlist(cfg.MountAllowlistPath)
		if err != nil {
			return err
		}
		// Credential handling. If the bundled credential proxy has an Anthropic
		// credential stored (goclaw auth add), route the runner through it: the
		// container gets ANTHROPIC_BASE_URL pointing at the host proxy and only a
		// PLACEHOLDER key, so the real key never enters the container (brief §8).
		// Otherwise fall back to passing a raw key (prefer the long-lived API key,
		// else the OAuth token). WithEnv drops empties.
		credStore := credstore.New(central.DB, cfg.SecretEncryptionKey)
		useProxy := false
		if credStore.HasKey() {
			if hosts, herr := credStore.Hosts(); herr == nil && hosts[anthropicHost] {
				useProxy = true
				proxyStore = credStore // signal the proxy goroutine to start
			}
		}
		claudeEnv := map[string]string{}
		if useProxy {
			claudeEnv["ANTHROPIC_BASE_URL"] = "http://host.docker.internal:" + cfg.CredProxyPort
			// The CLI requires a key to be present even with a base URL; it is sent
			// as x-api-key and the proxy swaps in the real one. So this is a decoy.
			claudeEnv["ANTHROPIC_API_KEY"] = "placeholder"
			log.Info("credential proxy active - real Anthropic key stays on the host",
				"base_url", claudeEnv["ANTHROPIC_BASE_URL"])
		} else if cfg.AnthropicAPIKey != "" {
			claudeEnv["ANTHROPIC_API_KEY"] = cfg.AnthropicAPIKey
		} else if cfg.ClaudeCodeOAuthToken != "" {
			claudeEnv["CLAUDE_CODE_OAUTH_TOKEN"] = cfg.ClaudeCodeOAuthToken
		}
		// Git identity, so the agent can commit (the vault repo, cloned repos).
		// git honors GIT_AUTHOR_*/GIT_COMMITTER_* without a writable git config.
		claudeEnv["GIT_AUTHOR_NAME"] = cfg.GitUserName
		claudeEnv["GIT_AUTHOR_EMAIL"] = cfg.GitUserEmail
		claudeEnv["GIT_COMMITTER_NAME"] = cfg.GitUserName
		claudeEnv["GIT_COMMITTER_EMAIL"] = cfg.GitUserEmail
		// GitHub auth for gh (clone private, push, fork, open PRs). Empty -> dropped.
		claudeEnv["GH_TOKEN"] = cfg.GitHubToken
		// Timezone: the container base image is UTC, so without this the agent's
		// clock (and any `date`) is hours off the user's wall time - it wrote
		// vault stamps on the wrong day and invalid hours like "24:30". Pass the
		// host's zone so the container, the agent, and the runner-injected "now"
		// all agree with the user. (Falls back to UTC if the host zone is unnamed.)
		if tz := hostTimezone(); tz != "" {
			claudeEnv["TZ"] = tz
		}
		// Transcript rotation thresholds (optional operator overrides; the runner
		// has sane defaults of 12MB / 14 days). Pass through only when set so the
		// runner's defaults apply otherwise. (WithEnv drops empties anyway.)
		claudeEnv["GOCLAW_TRANSCRIPT_ROTATE_BYTES"] = os.Getenv("GOCLAW_TRANSCRIPT_ROTATE_BYTES")
		claudeEnv["GOCLAW_TRANSCRIPT_ROTATE_AGE_DAYS"] = os.Getenv("GOCLAW_TRANSCRIPT_ROTATE_AGE_DAYS")
		mgr := runtime.New(cfg.PodmanBin, cfg.RunnerImage, runtime.RuntimeCrun, allow).
			WithEnv(claudeEnv).
			WithVault(cfg.VaultDir)
		ensurer = mgr
		runners = mgr
		log.Info("runner launch enabled", "image", cfg.RunnerImage,
			"mount_allowlist", cfg.MountAllowlistPath,
			"claude_auth", claudeAuthKind(cfg),
			"vault", vaultStatus(cfg.VaultDir))
		// A Claude Code OAuth token is short-lived (~12h) and the container can't
		// refresh it - it WILL eventually 401. Steer toward a long-lived API key.
		if cfg.ClaudeCodeOAuthToken != "" && cfg.AnthropicAPIKey == "" {
			log.Warn("using a Claude Code OAuth token - it expires in ~12h and the " +
				"container cannot refresh it; on 401 re-extract it, or set " +
				"GOCLAW_ANTHROPIC_API_KEY (long-lived) instead")
		}
	} else {
		log.Info("runner launch disabled - start a runner out of band (cmd/claude-runner)")
	}

	// Wire the host loops. errgroup ties their lifetimes to ctx: if any returns
	// a non-nil error, the group cancels and the rest unwind. The typing manager
	// is shared: the router starts the indicator, the delivery loop stops it.
	typer := typing.New(registry, log)
	rtr := router.New(central, cfg.DataDir, autoWireID, ensurer, registry, typer, log)
	del := delivery.New(central, registry, cfg.DataDir, typer, log)
	swp := sweep.New(central, cfg.DataDir, runners, log)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return rtr.Run(gctx, inbound) })
	g.Go(func() error { return del.Run(gctx) })
	g.Go(func() error { return swp.Run(gctx) })

	// Credential proxy (brief §8): runs only when an Anthropic credential is
	// stored and an encryption key is set. Listens on the host so runner
	// containers reach it at host.docker.internal:<port>; injects the real key
	// per request so the container only ever holds a placeholder.
	if proxyStore != nil {
		px := credproxy.New(proxyStore, anthropicHost, log)
		addr := "0.0.0.0:" + cfg.CredProxyPort
		g.Go(func() error { return px.Serve(gctx, addr) })
	}

	// Scheduled vault maintenance (brief §11.5): only when a vault is configured
	// and we know the owner (so jobs have a session to run in and a chat for the
	// summary). Targets the owner's DM with the default agent group.
	if cfg.VaultDir != "" && cfg.OwnerTelegramID != "" && ensurer != nil {
		target := maintenance.Target{
			AgentGroupID: agentGroupID,
			SessionKey:   "telegram:" + cfg.OwnerTelegramID,
			Channel:      "telegram",
			ChatID:       cfg.OwnerTelegramID,
		}
		sched := maintenance.New(central, cfg.DataDir, ensurer, target, log)
		g.Go(func() error { return sched.Run(gctx) })
		log.Info("vault maintenance enabled", "owner", cfg.OwnerTelegramID)
	}

	log.Info("goclaw host started")
	err = g.Wait()

	// On shutdown, stop the runner containers we launched so they don't outlive
	// the host. Use a fresh context - the root ctx is already cancelled.
	if runners != nil {
		stopRunners(runners, log)
	}

	if errors.Is(err, context.Canceled) {
		return nil // clean shutdown
	}
	return err
}

// vaultStatus reports the vault dir for the startup log, or "disabled".
func vaultStatus(dir string) string {
	if dir == "" {
		return "disabled"
	}
	return dir
}

// claudeAuthKind reports which Claude credential the runner will use, for the
// startup log. An API key wins (it's long-lived); the OAuth token is the
// short-lived fallback.
func claudeAuthKind(cfg *config.Config) string {
	switch {
	case cfg.AnthropicAPIKey != "":
		return "api-key"
	case cfg.ClaudeCodeOAuthToken != "":
		return "oauth-token (short-lived)"
	default:
		return "none"
	}
}

// stopRunners stops every running runner container during host shutdown.
func stopRunners(runners sweep.RunnerManager, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ids, err := runners.RunningGroupIDs(ctx)
	if err != nil {
		log.Error("shutdown: list runners", "err", err)
		return
	}
	for _, id := range ids {
		if err := runners.StopGroupRunner(ctx, id); err != nil {
			log.Error("shutdown: stop runner", "agent_group", id, "err", err)
			continue
		}
		log.Info("shutdown: stopped runner", "agent_group", id)
	}
}

// hostTimezone returns the host's IANA timezone name (e.g. "America/Los_Angeles")
// for passing to the container as TZ, so the agent's clock matches the user's
// wall time instead of the base image's UTC. Resolution order: the TZ env var if
// it already names a zone, else the target of the /etc/localtime symlink (the
// path segment after ".../zoneinfo/"). Returns "" if neither yields a name, in
// which case the caller leaves TZ unset (container stays UTC).
func hostTimezone() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}
	target, err := filepath.EvalSymlinks("/etc/localtime")
	if err != nil {
		return ""
	}
	if i := strings.LastIndex(target, "zoneinfo/"); i >= 0 {
		return target[i+len("zoneinfo/"):]
	}
	return ""
}
