// Command goclaw is the host orchestrator entry point. It loads config,
// initializes the central DB, registers channel adapters, and starts the
// router, delivery, and sweep loops under a single cancellable context. A
// SIGINT/SIGTERM cancels that context and every loop unwinds for graceful
// shutdown (brief §5.1, §10).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/shindakun/goclaw/internal/channels"
	"github.com/shindakun/goclaw/internal/channels/discord"
	chanplugin "github.com/shindakun/goclaw/internal/channels/plugin"
	"github.com/shindakun/goclaw/internal/channels/telegram"
	"github.com/shindakun/goclaw/internal/config"
	"github.com/shindakun/goclaw/internal/credproxy"
	"github.com/shindakun/goclaw/internal/credstore"
	"github.com/shindakun/goclaw/internal/db"
	"github.com/shindakun/goclaw/internal/delivery"
	"github.com/shindakun/goclaw/internal/eventlog"
	"github.com/shindakun/goclaw/internal/maintenance"
	"github.com/shindakun/goclaw/internal/mounts"
	"github.com/shindakun/goclaw/internal/outscan"
	"github.com/shindakun/goclaw/internal/plugin"
	"github.com/shindakun/goclaw/internal/router"
	"github.com/shindakun/goclaw/internal/runtime"
	"github.com/shindakun/goclaw/internal/scheduler"
	"github.com/shindakun/goclaw/internal/sweep"
	"github.com/shindakun/goclaw/internal/typing"
)

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
	defer func() { _ = central.Close() }()
	log.Info("central db ready", "path", cfg.CentralDBPath)

	// Operational event log: a host-owned, append-only, structured record of what the
	// host DID (schedule fire/defer, delivery sent/denied/failed, proxy CA minted,
	// runner launched/reaped), retained and queryable unlike the ephemeral slog stderr.
	// The host is the SOLE writer; it is mounted READ-ONLY into the runner (gated on a
	// single agent group, see below) so the agent's introspection skill can read it
	// without any write channel back to the host. A construction failure is non-fatal:
	// the host runs without the event log rather than refusing to start over a logging
	// concern.
	events, err := eventlog.New(filepath.Join(cfg.DataDir, "events"), eventlog.Config{}, log)
	if err != nil {
		log.Warn("event log disabled (construction failed)", "err", err)
		events = nil
	}

	// Optional startup seeding so a first user can message the host without
	// hand-editing the DB (brief §3.4). Idempotent.
	_, agentGroupID, err := central.Apply(db.Bootstrap{
		OwnerTelegramID:         cfg.OwnerTelegramID,
		OwnerDiscordID:          cfg.OwnerDiscordID,
		DefaultAgentGroupName:   cfg.DefaultAgentGroupName,
		DefaultAgentGroupFolder: cfg.DefaultAgentGroupFolder,
	})
	if err != nil {
		return err
	}
	if cfg.OwnerTelegramID != "" {
		log.Info("seeded owner", "telegram_id", cfg.OwnerTelegramID)
	}
	if cfg.OwnerDiscordID != "" {
		log.Info("seeded owner", "discord_id", cfg.OwnerDiscordID)
	}

	// Owner auto-wiring is opt-in and only meaningful with a default agent group.
	var autoWireID int64
	if cfg.AutoWireOwner {
		autoWireID = agentGroupID
		log.Info("owner auto-wire enabled", "agent_group", autoWireID)
	}

	// Channel registry. Each channel registers only when its token is configured
	// (brief §7.4); they all implement the same ChannelAdapter interface.
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
	}
	var discordAdapter *discord.Adapter
	if cfg.DiscordToken != "" {
		dc, err := discord.New(cfg.DiscordToken)
		if err != nil {
			return err
		}
		if err := registry.Register(dc); err != nil {
			return err
		}
		discordAdapter = dc
		log.Info("registered channel", "channel", dc.Name())
	}
	// Channel PLUGINS (kind: channel) register here too, but only when the runner is
	// enabled: the plugin runs IN the container and the host connects to it over the
	// boundary socket, so without a runner there is nothing to connect to. The relay
	// binds each socket now (before StartAll) and waits for the in-container runner to
	// dial in; the container launches lazily on the first message, and the runner
	// retries its dial, so listening up front is correct. A relay failure for one
	// channel is logged and skipped, never fatal.
	var chanRelay *chanplugin.Relay
	if cfg.LaunchRunner {
		chanRelay, err = setupChannelPlugins(registry, channelRelayConfig(cfg), pluginsHostDir(cfg), events, log)
		if err != nil {
			return err
		}
		defer chanRelay.CloseAll()
	}

	if len(registry.All()) == 0 {
		log.Warn("no channel tokens set (TELEGRAM_BOT_TOKEN / GOCLAW_DISCORD_TOKEN) - no channels registered")
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
		// proxyStore is non-nil when the bundled credential proxy should run (a
		// credential is stored + an encryption key is set). The proxy goroutine
		// below uses it; nil means no proxy (raw key path).
		proxyStore *credstore.Store
		// proxyCAHostPath is the host path to the proxy CA cert, mounted into the
		// container so its tools trust the intercepted TLS. Set when useProxy.
		proxyCAHostPath string
		// proxyCA is the built CA, shared between the spawn wiring and the proxy
		// goroutine so they use the same root. nil when the proxy is off.
		proxyCA *credproxy.CA
		// outboundNeedles are the exact real secret VALUES that actually enter the
		// runner container's env, fed to the outbound content scanner as literal
		// needles so a reply echoing one is blocked. Populated only on the raw-key
		// path: with the proxy active the container holds "placeholder", never a real
		// token, so there is nothing real to leak and the list stays empty.
		outboundNeedles []string
	)
	if cfg.LaunchRunner {
		// Load the external mount allowlist (fail-closed if absent) to validate
		// any extra group mounts at launch (brief §6.3).
		allow, err := mounts.LoadAllowlist(cfg.MountAllowlistPath)
		if err != nil {
			return err
		}
		// Credential handling (brief §8). When an encryption key is set and at
		// least one credential is stored (goclaw auth add), route the runner's
		// HTTPS through the bundled TLS-intercepting proxy: the container trusts
		// the proxy's CA and the proxy injects the real token per request, so no
		// raw token (Anthropic or GitHub) enters the container. Otherwise fall back
		// to passing raw keys (prefer the long-lived API key, else the OAuth token,
		// plus GH_TOKEN). WithEnv drops empties.
		credStore := credstore.New(central.DB, cfg.SecretEncryptionKey)
		useProxy := false
		if credStore.HasKey() {
			if hosts, herr := credStore.Hosts(); herr == nil && len(hosts) > 0 {
				useProxy = true
				proxyStore = credStore // signal the proxy goroutine to start
			}
		}
		claudeEnv := map[string]string{}
		if useProxy {
			// Load the proxy CA (or generate one only if none exists). LoadOrGenerateCA
			// persists the cert itself when it has to (re-derive or generate), and is
			// designed to keep the CA IDENTITY stable across restarts. We deliberately do
			// NOT rewrite ca.pem here on every start: the CA is mounted into running
			// containers as a single-file bind mount, and a gratuitous rewrite can change
			// the file inode under that live mount, leaving containers trusting a stale cert
			// (the "tls: bad certificate" flood). Stable key -> stable cert -> stable mount.
			ca, generated, err := credproxy.LoadOrGenerateCA(proxyCADir(cfg), cfg.ProxyCAKey, cfg.ProxyCACert)
			if err != nil {
				return fmt.Errorf("credential proxy CA: %w", err)
			}
			if generated {
				// A NEW CA identity was minted (no usable key existed). This invalidates the
				// CA any already-running container trusts; surface it loudly and actionably
				// rather than letting the proxy silently flood bad-certificate warnings.
				log.Warn("credential proxy: generated a NEW CA identity",
					"impact", "any already-running runner container trusts a stale CA; recreate runners so they remount the new cert",
					"dir", proxyCADir(cfg))
				events.Emit(eventlog.KindProxyCANew, nil, map[string]any{
					"dir":    proxyCADir(cfg),
					"impact": "running runners trust a stale CA until recreated",
				})
			}
			caPath := filepath.Join(proxyCADir(cfg), "ca.pem") // mount source (NOT rewritten here)
			proxyCA = ca
			proxyURL := "http://host.docker.internal:" + cfg.CredProxyPort
			caCont := runtime.CACertContainerPath()
			// Route HTTPS through the proxy; Node needs NODE_USE_ENV_PROXY. Reach
			// the proxy itself directly (NO_PROXY) so the CONNECT is not proxied.
			// Set BOTH cases: curl/Node honor the uppercase form, but git (libcurl)
			// only reads the lowercase https_proxy/http_proxy - without these git
			// connects directly and never gets the injected credential.
			noProxy := "host.docker.internal,localhost,127.0.0.1"
			claudeEnv["HTTPS_PROXY"] = proxyURL
			claudeEnv["HTTP_PROXY"] = proxyURL
			claudeEnv["https_proxy"] = proxyURL
			claudeEnv["http_proxy"] = proxyURL
			claudeEnv["NODE_USE_ENV_PROXY"] = "1"
			claudeEnv["NO_PROXY"] = noProxy
			claudeEnv["no_proxy"] = noProxy
			// Trust the proxy CA across the agent's tools.
			claudeEnv["NODE_EXTRA_CA_CERTS"] = caCont // claude CLI (Node)
			claudeEnv["SSL_CERT_FILE"] = caCont       // curl, python, Go
			claudeEnv["GIT_SSL_CAINFO"] = caCont      // git (gh uses git's stack)
			// The CLI still wants a key present even though the proxy supplies the
			// real one; a decoy is fine (the proxy swaps it for the stored key).
			claudeEnv["ANTHROPIC_API_KEY"] = "placeholder"
			// gh refuses to run most subcommands unless it sees a token (it checks
			// "gh auth status" locally before any network call). Give it a
			// PLACEHOLDER when a GitHub credential is stored: gh considers itself
			// logged in and sends the placeholder to api.github.com, where the proxy
			// swaps in the real token. The real token never enters the container.
			if hosts, _ := credStore.Hosts(); hosts["api.github.com"] || hosts["github.com"] {
				claudeEnv["GH_TOKEN"] = "placeholder"
			}
			proxyCAHostPath = caPath
			log.Info("credential proxy active - raw tokens stay on the host",
				"proxy", proxyURL, "credentials", credHostList(credStore))
		} else if cfg.AnthropicAPIKey != "" {
			claudeEnv["ANTHROPIC_API_KEY"] = cfg.AnthropicAPIKey
			outboundNeedles = append(outboundNeedles, cfg.AnthropicAPIKey)
		} else if cfg.ClaudeCodeOAuthToken != "" {
			claudeEnv["CLAUDE_CODE_OAUTH_TOKEN"] = cfg.ClaudeCodeOAuthToken
			outboundNeedles = append(outboundNeedles, cfg.ClaudeCodeOAuthToken)
		}
		// Git identity, so the agent can commit (the vault repo, cloned repos).
		// git honors GIT_AUTHOR_*/GIT_COMMITTER_* without a writable git config.
		claudeEnv["GIT_AUTHOR_NAME"] = cfg.GitUserName
		claudeEnv["GIT_AUTHOR_EMAIL"] = cfg.GitUserEmail
		claudeEnv["GIT_COMMITTER_NAME"] = cfg.GitUserName
		claudeEnv["GIT_COMMITTER_EMAIL"] = cfg.GitUserEmail
		// GitHub auth: pass the raw GH_TOKEN ONLY when the proxy is NOT injecting
		// it (no credential stored). With the proxy active the token stays on the
		// host and is injected per request, so we must not also leak it here.
		if !useProxy {
			claudeEnv["GH_TOKEN"] = cfg.GitHubToken
			outboundNeedles = append(outboundNeedles, cfg.GitHubToken)
		}
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
			WithVault(cfg.VaultDir).
			WithCredCA(proxyCAHostPath).      // empty when the proxy is off
			WithPlugins(pluginsHostDir(cfg)). // <data>/plugins, mounted RO at /plugins
			WithEventLog(events)              // emits runner.launched on actual (re)launch (nil-safe)
		// The channel-socket mount is only needed for the "unix" channel transport; the
		// "tcp" transport (default, for macOS) dials host.docker.internal and needs no
		// mounted socket.
		if cfg.ChannelTransport == "unix" {
			mgr = mgr.WithChannelSockets(channelSocketsHostDir(cfg))
		}
		// Mount the operational event log READ-ONLY into the container so the agent's
		// introspection skill can read it. Gated FAIL-CLOSED on there being a single
		// agent group: the one shared log can contain events about every group, so
		// mounting it into a group's container is only safe when there is just one
		// group (nothing another group owns can leak in). With more than one group we
		// log and DO NOT mount, until a per-group event log is built (RFC
		// event-log-and-introspection §7 q1). Skipped silently if the event log is off.
		if events != nil {
			groupCount, cerr := central.CountAgentGroups()
			switch {
			case cerr != nil:
				log.Warn("events mount skipped (could not count agent groups)", "err", cerr)
			case groupCount == 1:
				mgr = mgr.WithEvents(filepath.Join(cfg.DataDir, "events"))
				log.Info("event log mounted read-only into the runner", "path", "/run/goclaw/events")
			default:
				log.Warn("event log NOT mounted: multiple agent groups exist, and the single "+
					"shared log can contain other groups' events; per-group event logs are not "+
					"built yet (RFC event-log-and-introspection q1)", "agent_groups", groupCount)
			}
		}
		ensurer = mgr
		runners = mgr
		log.Info("runner launch enabled", "image", cfg.RunnerImage,
			"mount_allowlist", cfg.MountAllowlistPath,
			"claude_auth", claudeAuthKind(cfg),
			"vault", vaultStatus(cfg.VaultDir))
		// A channel plugin runs IN the container and must connect to its upstream (e.g.
		// IRC) as soon as the host is up, not wait for some unrelated first message. So
		// when channel plugins are installed, eagerly launch the default agent group's
		// container at startup: its in-container runner discovers the channel plugins and
		// they dial out immediately. Done in the background so a slow podman launch does
		// not block host startup; a failure is logged, and the normal on-message launch
		// path still applies.
		if chanRelay != nil && chanRelay.OpenCount() > 0 {
			go func() {
				groupDir := db.AgentGroupDir(cfg.DataDir, agentGroupID)
				if err := mgr.EnsureRunner(ctx, agentGroupID, groupDir); err != nil {
					log.Error("eager runner launch for channel plugins failed", "agent_group", agentGroupID, "err", err)
					return
				}
				log.Info("eager runner launched for channel plugins", "agent_group", agentGroupID)
			}()
		}
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
	// The router owns the slash-command registry (built-in /commands, /approve,
	// the /reset & /compact pass-throughs). Passing nil lets it create one; the
	// plugin manager will register plugin commands into rtr.Commands() later.
	rtr := router.New(central, cfg.DataDir, autoWireID, ensurer, registry, typer, nil, log)
	// The router intercepts an agent reply that IS a "/schedule ..." directive, so the
	// agent can manage scheduled tasks by emitting one (natural-language scheduling).
	// Outbound content scanner: defense-in-depth on the send side (bounds what the
	// agent can put on the wire, beneath containment which bounds what it can reach).
	// Seeded with the exact real secret values that entered the container env on the
	// raw-key path (empty on the proxy path, where only a placeholder is present).
	del := delivery.New(central, registry, cfg.DataDir, typer, log).
		WithInterceptor(rtr).
		WithScanner(outscan.New(outboundNeedles...)).
		WithEventLog(events)
	swp := sweep.New(central, cfg.DataDir, runners, log).WithEventLog(events)
	// Pin the channel-hosting agent group so the sweep never reaps its container as
	// idle: an always-on channel plugin (e.g. IRC) must keep its container running to
	// stay connected, even with no recent agent activity.
	if chanRelay != nil && chanRelay.OpenCount() > 0 {
		swp = swp.WithPinnedGroups(agentGroupID)
	}

	// Plugins run INSIDE the agent container, not on the host. The host mounts the
	// <data>/plugins dir read-only at /plugins (WithPlugins above); the in-container
	// runner (cmd/claude-runner) discovers and launches them, exposing their tools
	// to the agent. The host deliberately does NOT execute plugin binaries: they are
	// untrusted downloaded code and must stay in the sandbox. (Slash-command routing
	// to plugins goes inward through the session DBs; see docs/plugins-design.md.)
	//
	// The host still reads each plugin.yml under <data>/plugins to LIST each plugin's
	// slash command in /commands (as a pass-through it does not execute), so plugin
	// commands stay discoverable even though the runner is what runs them.
	rtr.RegisterPluginCommands(pluginsHostDir(cfg))

	// The owner-only /plugin command (add/list/remove). It builds a plugin inside a
	// throwaway container (untrusted source never compiles on the host) and stages
	// the artifact into <data>/plugins, where the in-container runner's watch loads
	// it live. Only available when the runner is enabled (it needs the runner image
	// for the sandboxed build).
	if cfg.LaunchRunner {
		installer := plugin.NewInstaller(pluginsHostDir(cfg), cfg.RunnerImage, cfg.PodmanBin).WithEventLog(events)
		rtr.SetInstaller(installer, pluginsHostDir(cfg))
		// Let a kind:channel plugin installed via `/plugin add` activate live (bind a
		// relay socket + register its adapter) instead of waiting for the next restart.
		// Needs the relay, which exists whenever channel-plugin support is wired.
		if chanRelay != nil {
			rtr.WithChannelActivator(&channelActivator{relay: chanRelay, registry: registry})
		}
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return rtr.Run(gctx, inbound) })
	g.Go(func() error { return del.Run(gctx) })
	g.Go(func() error { return swp.Run(gctx) })

	// Credential proxy (brief §8): runs only when a credential is stored and an
	// encryption key is set. The TLS-intercepting proxy listens on the host so
	// runner containers reach it at host.docker.internal:<port>; it injects the
	// real token per request so the container only ever holds a placeholder and
	// trusts the proxy CA.
	if proxyStore != nil && proxyCA != nil {
		px := credproxy.NewMITM(proxyStore, proxyCA, log)
		addr := "0.0.0.0:" + cfg.CredProxyPort
		g.Go(func() error { return px.Serve(gctx, addr) })
	}

	// Scheduled vault maintenance (brief §11.5): only when a vault is configured
	// and we know an owner channel to post summaries to. Prefer Telegram (the
	// owner id is also the DM chat id); else Discord (resolve the owner's DM
	// channel, since Discord posts to channels, not user ids).
	if cfg.VaultDir != "" && ensurer != nil {
		target, ok := maintenanceTarget(cfg, agentGroupID, discordAdapter, log)
		if ok {
			sched := maintenance.New(central, cfg.DataDir, ensurer, target, log).WithEventLog(events)
			g.Go(func() error { return sched.Run(gctx) })
			log.Info("vault maintenance enabled", "channel", target.Channel, "chat", target.ChatID)
		}
	}

	// User-definable scheduled tasks (docs/scheduled-tasks.md): a daily/weekly/interval
	// task enqueues its prompt into its own target session and ensures the runner. Runs
	// whenever the runner is enabled (it needs to wake the container). Owner-gated via
	// the /schedule command and the schedule_* agent tools.
	if ensurer != nil {
		tsk := scheduler.New(central, cfg.DataDir, ensurer, log).WithEventLog(events)
		g.Go(func() error { return tsk.Run(gctx) })
		log.Info("task scheduler enabled")
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

// maintenanceTarget builds the scheduled-maintenance target from whichever owner
// channel is configured. Telegram is preferred (the owner id is also the DM chat
// id). For Discord it resolves the owner's DM channel, since Discord posts to a
// channel id, not a user id. Returns ok=false if no owner channel is configured.
func maintenanceTarget(cfg *config.Config, agentGroupID int64, dc *discord.Adapter, log *slog.Logger) (maintenance.Target, bool) {
	if cfg.OwnerTelegramID != "" {
		return maintenance.Target{
			AgentGroupID: agentGroupID,
			SessionKey:   "telegram:" + cfg.OwnerTelegramID,
			Channel:      "telegram",
			ChatID:       cfg.OwnerTelegramID,
		}, true
	}
	if cfg.OwnerDiscordID != "" && dc != nil {
		dmID, err := dc.DMChannelID(cfg.OwnerDiscordID)
		if err != nil {
			log.Warn("vault maintenance: could not open owner DM on Discord - maintenance disabled", "err", err)
			return maintenance.Target{}, false
		}
		// Session keys on the owner's user id (stable identity); the reply is
		// delivered to the resolved DM channel id.
		return maintenance.Target{
			AgentGroupID: agentGroupID,
			SessionKey:   "discord:" + cfg.OwnerDiscordID,
			Channel:      "discord",
			ChatID:       dmID,
		}, true
	}
	return maintenance.Target{}, false
}

// proxyCADir is where the credential proxy persists its auto-generated CA when
// not supplied via env: {data_dir}/proxy. The dir is created by the CA loader.
func proxyCADir(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, "proxy")
}

// pluginsHostDir is where installed plugins live on the host: {data_dir}/plugins.
// Each plugin is a subdir with its binary + plugin.yml. The host mounts this dir
// read-only into the agent container at /plugins; the in-container runner launches
// the plugins. It lives under the data dir (runtime state), not the repo root.
func pluginsHostDir(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, "plugins")
}

// channelSocketsHostDir is the host dir holding per-channel Unix sockets, mounted RW
// into the container at /run/goclaw/channels. Used only by the "unix" channel transport.
// Lives under the data dir.
func channelSocketsHostDir(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, "run", "channels")
}

// channelRelayConfig builds the channel relay's transport config from cfg. "tcp" (the
// default) has the host bind TCP and the container dial host.docker.internal, which works
// on macOS+podman-VM where a mounted Unix socket connect fails; "unix" uses the mounted
// socket dir (native Linux).
func channelRelayConfig(cfg *config.Config) chanplugin.Config {
	if cfg.ChannelTransport == "unix" {
		return chanplugin.Config{Transport: chanplugin.TransportUnix, SockDir: channelSocketsHostDir(cfg)}
	}
	return chanplugin.Config{Transport: chanplugin.TransportTCP, TCPHost: "host.docker.internal"}
}

// setupChannelPlugins discovers installed kind:channel plugins, binds a host relay
// socket for each, and registers the resulting adapter so the router/agent treat it
// like a built-in channel. The relay listens now and accepts the in-container runner's
// dial in the background (the container launches lazily). A failure for one channel is
// logged and skipped, never fatal; the returned relay is closed on host shutdown.
func setupChannelPlugins(registry *channels.Registry, cfg chanplugin.Config, pluginsDir string, events *eventlog.Logger, log *slog.Logger) (*chanplugin.Relay, error) {
	relay, err := chanplugin.NewRelay(cfg, log)
	if err != nil {
		return nil, fmt.Errorf("channel relay: %w", err)
	}
	relay = relay.WithEventLog(events) // records channel.attached/detached (nil-safe)
	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return relay, nil // no plugins installed yet
		}
		return nil, fmt.Errorf("read plugins dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		pdir := filepath.Join(pluginsDir, e.Name())
		man, err := plugin.LoadManifest(pdir)
		if err != nil || man.Kind != "channel" {
			continue // not a (valid) channel plugin
		}
		if err := registerChannelPlugin(relay, registry, man.Name, pdir); err != nil {
			log.Error("channel plugin: register failed at startup", "channel", man.Name, "err", err)
			continue
		}
		log.Info("registered channel plugin", "channel", man.Name)
	}
	return relay, nil
}

// registerChannelPlugin binds a relay socket for one channel plugin and registers
// the resulting adapter, so the router/agent treat it like a built-in channel. It
// is the shared per-plugin step used both by startup discovery (setupChannelPlugins)
// and by live activation after `/plugin add` (channelActivator). pdir is the host
// plugin dir: the relay writes its .endpoint there, and the runner reads it from
// the same dir mounted read-only into the container.
func registerChannelPlugin(relay *chanplugin.Relay, registry *channels.Registry, name, pdir string) error {
	adapter, err := relay.Open(name, pdir)
	if err != nil {
		return fmt.Errorf("relay open: %w", err)
	}
	if err := registry.Register(adapter); err != nil {
		relay.Close(name) // unwind the listener we just bound
		return fmt.Errorf("register adapter: %w", err)
	}
	return nil
}

// channelActivator implements router.ChannelActivator, letting a kind:channel
// plugin installed via `/plugin add` start routing without a host restart. It
// holds the same relay + registry the startup discovery uses, so live activation
// is exactly the startup path applied to one plugin.
type channelActivator struct {
	relay    *chanplugin.Relay
	registry *channels.Registry
}

// Activate binds a relay socket and registers the channel plugin's adapter live.
func (a *channelActivator) Activate(name, pluginDir string) error {
	return registerChannelPlugin(a.relay, a.registry, name, pluginDir)
}

// Deactivate unregisters the channel's adapter and tears down its relay socket. It
// is a no-op for a name that is not an open channel, so callers may invoke it
// unconditionally (e.g. on any plugin remove, or before re-activating on update).
func (a *channelActivator) Deactivate(name string) {
	a.registry.Unregister(name)
	a.relay.Close(name)
}

// credHostList returns the stored credential hosts for a startup log line, so the
// operator can see what the proxy will inject for (no tokens, just hosts).
func credHostList(s *credstore.Store) string {
	hosts, err := s.Hosts()
	if err != nil || len(hosts) == 0 {
		return "(none)"
	}
	out := make([]string, 0, len(hosts))
	for h := range hosts {
		out = append(out, h)
	}
	return strings.Join(out, ",")
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
