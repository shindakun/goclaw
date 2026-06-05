// Command chantest is a DEV-ONLY harness for bringing up a channel plugin on the host
// directly, with no container, no socket boundary, and no agent: it launches the
// plugin binary, speaks the host side of the channel.* protocol to it (via
// internal/plugin.ChannelClient), prints every inbound message the plugin pushes up,
// and optionally echoes a canned reply back. It exists to answer one question early:
// does a channel plugin (e.g. the goclawkit IRC bridge) actually connect to its
// upstream and relay messages, before the full sandboxed relay path is built?
//
// It is NOT part of the running host. It does NOT pass the host's full environment to
// the plugin: only the plugin manifest's declared env: names cross, on top of a
// secret-free PATH-only base (the same allowlist rule the real launcher uses), so host
// secrets never leak to the plugin. Config for the plugin comes from a .env file
// (optional) and/or the current environment, resolved by the manifest allowlist.
//
// Usage:
//
//	go run ./cmd/chantest -plugin ../goclawkit/cmd/irc            # build+run that plugin dir
//	go run ./cmd/chantest -bin /path/to/irc -manifest /path/to/plugin.yml
//	go run ./cmd/chantest -plugin ../goclawkit/cmd/irc -echo "pong from chantest"
//
// With no env set the IRC plugin defaults to irc.libera.chat:6697 / goclawbot /
// #goclawtester, so a bare run is a real connectivity test against Libera.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/shindakun/goclaw/internal/plugin"
)

func main() {
	var (
		pluginDir = flag.String("plugin", "", "path to a plugin source dir (built with `go build` here) containing plugin.yml")
		binPath   = flag.String("bin", "", "path to a prebuilt plugin binary (use with -manifest)")
		manPath   = flag.String("manifest", "", "path to plugin.yml (used with -bin)")
		envFile   = flag.String("env", ".env", "optional .env file to load plugin config from")
		echo      = flag.String("echo", "", "if set, reply to every inbound with this text (back to its ChatID)")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(*pluginDir, *binPath, *manPath, *envFile, *echo, log); err != nil {
		fmt.Fprintln(os.Stderr, "chantest:", err)
		os.Exit(1)
	}
}

func run(pluginDir, binPath, manPath, envFile, echo string, log *slog.Logger) error {
	// Load .env (best-effort) into THIS process's env so the manifest allowlist can
	// resolve plugin config from it. This is the host's config, exactly like the real
	// host loads .env; only allowlisted names are then handed to the plugin.
	if err := loadDotEnv(envFile); err != nil {
		return fmt.Errorf("load %s: %w", envFile, err)
	}

	bin, manifestDir, err := resolveBinary(pluginDir, binPath, manPath)
	if err != nil {
		return err
	}
	man, err := plugin.LoadManifest(manifestDir)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}
	if man.Kind != "channel" {
		return fmt.Errorf("plugin %q is kind %q, chantest only drives channels", man.Name, man.Kind)
	}

	// Only the manifest's declared env names cross, on a PATH-only base. NEVER
	// os.Environ(): the host env may hold secrets that must not reach the plugin.
	env := man.InjectEnv(plugin.MinimalEnvBase(), os.LookupEnv)
	log.Info("launching channel plugin", "name", man.Name, "bin", bin, "declared_env", man.Env)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c, err := plugin.LaunchChannel(ctx, man.Name, bin, env, log)
	if err != nil {
		return fmt.Errorf("launch channel: %w", err)
	}
	defer func() { _ = c.Close() }()

	log.Info("channel up; waiting for inbound (Ctrl-C to stop)", "kind", c.Info().Kind, "version", c.Info().Version)

	for {
		select {
		case in, ok := <-c.Inbound():
			if !ok {
				log.Info("inbound stream closed (plugin exited)")
				return nil
			}
			fmt.Printf("INBOUND  chat=%s sender=%s(%s) text=%q\n", in.ChatID, in.Sender, in.SenderID, in.Text)
			if echo != "" {
				out := plugin.ChannelOutbound{Channel: in.Channel, ChatID: in.ChatID, Text: echo}
				sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				err := c.SendOutbound(sendCtx, out)
				cancel()
				if err != nil {
					log.Error("echo send failed", "err", err)
				} else {
					fmt.Printf("OUTBOUND chat=%s text=%q\n", in.ChatID, echo)
				}
			}
		case <-ctx.Done():
			log.Info("shutting down")
			return nil
		}
	}
}

// resolveBinary returns (binaryPath, manifestDir). With -plugin it builds the dir's
// binary into a temp path and uses that dir's plugin.yml. With -bin/-manifest it uses
// them directly.
func resolveBinary(pluginDir, binPath, manPath string) (string, string, error) {
	switch {
	case pluginDir != "":
		man, err := plugin.LoadManifest(pluginDir)
		if err != nil {
			return "", "", fmt.Errorf("read plugin.yml in %s: %w", pluginDir, err)
		}
		out := filepath.Join(os.TempDir(), "chantest-"+man.Name)
		// Build the plugin dir. `go build .` in the dir produces a host binary, which
		// is what we want for a direct (no-container) connectivity test.
		cmd := exec.Command("go", "build", "-o", out, ".")
		cmd.Dir = pluginDir
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return "", "", fmt.Errorf("build %s: %w", pluginDir, err)
		}
		return out, pluginDir, nil
	case binPath != "" && manPath != "":
		return binPath, filepath.Dir(manPath), nil
	default:
		return "", "", fmt.Errorf("need -plugin <dir>, or -bin <binary> with -manifest <plugin.yml>")
	}
}

// loadDotEnv loads KEY=VALUE lines from path into the process environment, with real
// environment variables taking precedence (a var already set is not overwritten). A
// missing file is not an error. This is a minimal local loader so the dev harness does
// not depend on the host's full config.Load.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
	return sc.Err()
}
