// Package config loads host configuration from the environment. Kept tiny and
// explicit in the spirit of "few files, easy to read" (brief §10).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config holds the host's runtime configuration.
type Config struct {
	// DataDir is the root for the central DB and per-session DBs.
	DataDir string
	// CentralDBPath is the path to the central database file.
	CentralDBPath string
	// MountAllowlistPath is the external allowlist (outside the project root).
	MountAllowlistPath string
	// TelegramToken is the Telegram bot token (empty disables the channel).
	TelegramToken string
	// PodmanBin is the podman binary name/path.
	PodmanBin string
	// LaunchRunner enables host-managed runner containers: on enqueue the host
	// runs RunnerImage in Podman for the session. When false, the runner is
	// expected to be started out of band (e.g. cmd/stub-runner by hand).
	LaunchRunner bool
	// RunnerImage is the container image launched per agent group when
	// LaunchRunner is on (build it from one of the container/*.Containerfile).
	RunnerImage string
	// AnthropicAPIKey, if set, is passed into the runner container as
	// ANTHROPIC_API_KEY so the Claude runner can authenticate. Use a standard
	// API key (sk-ant-api…). Empty for the echo stub.
	AnthropicAPIKey string
	// ClaudeCodeOAuthToken, if set, is passed in as CLAUDE_CODE_OAUTH_TOKEN so
	// the Claude runner authenticates with a Claude Code subscription instead of
	// an API key. On macOS the host token lives in the Keychain ("Claude
	// Code-credentials"); extract it into this env var. Takes precedence over
	// AnthropicAPIKey when both are set.
	ClaudeCodeOAuthToken string
	// VaultDir, if set, is a host directory mounted read-write at /vault in the
	// agent-group container, and the runner reads /vault/CLAUDE.md as its system
	// prompt (brief §11). Empty disables the knowledge vault. Create one with
	// `goclaw vault init`.
	VaultDir string
	// GitUserName / GitUserEmail set the agent's git identity inside the
	// container (so `git commit` works on the vault and on cloned repos). Passed
	// in as GIT_AUTHOR_*/GIT_COMMITTER_* env. Sensible defaults applied.
	GitUserName  string
	GitUserEmail string
	// GitHubToken, if set, is passed into the container as GH_TOKEN so the agent
	// can clone private repos, push branches, fork, and open pull requests with
	// the gh CLI. Empty leaves the agent able to clone public repos only.
	GitHubToken string
	// OwnerTelegramID, if set, is seeded at startup as the owner user's Telegram
	// identity so the first real user can get past the access gate without
	// hand-editing the DB (brief §3.4). Empty disables the bootstrap.
	OwnerTelegramID string
	// AutoWireOwner, when true, auto-wires an unwired conversation the owner
	// messages to the default agent group. Convenience for first-run only.
	AutoWireOwner bool
	// DefaultAgentGroup names the agent group seeded at startup and used for
	// owner auto-wiring.
	DefaultAgentGroupName   string
	DefaultAgentGroupFolder string
}

// Load reads configuration from environment variables, applying defaults.
// It first loads a local .env file (if present) into the environment, with real
// environment variables taking precedence. See .env.example for the keys.
func Load() (*Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return nil, fmt.Errorf("config: load .env: %w", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("config: home dir: %w", err)
	}

	dataDir := getenv("GOCLAW_DATA_DIR", "data")
	cfg := &Config{
		DataDir:                 dataDir,
		CentralDBPath:           filepath.Join(dataDir, "goclaw.db"),
		MountAllowlistPath:      getenv("GOCLAW_MOUNT_ALLOWLIST", filepath.Join(home, ".config", "goclaw", "mount-allowlist.json")),
		TelegramToken:           os.Getenv("TELEGRAM_BOT_TOKEN"),
		PodmanBin:               getenv("GOCLAW_PODMAN_BIN", "podman"),
		OwnerTelegramID:         os.Getenv("GOCLAW_OWNER_TELEGRAM_ID"),
		AutoWireOwner:           os.Getenv("GOCLAW_AUTO_WIRE_OWNER") == "1",
		DefaultAgentGroupName:   getenv("GOCLAW_DEFAULT_AGENT_GROUP", "default"),
		DefaultAgentGroupFolder: getenv("GOCLAW_DEFAULT_AGENT_GROUP_FOLDER", "default"),
		LaunchRunner:            os.Getenv("GOCLAW_LAUNCH_RUNNER") == "1",
		RunnerImage:             getenv("GOCLAW_RUNNER_IMAGE", "goclaw-runner:latest"),
		AnthropicAPIKey:         os.Getenv("GOCLAW_ANTHROPIC_API_KEY"),
		ClaudeCodeOAuthToken:    os.Getenv("GOCLAW_CLAUDE_CODE_OAUTH_TOKEN"),
		VaultDir:                expandHome(os.Getenv("GOCLAW_VAULT_DIR"), home),
		GitUserName:             getenv("GOCLAW_GIT_USER_NAME", "goclaw agent"),
		GitUserEmail:            getenv("GOCLAW_GIT_USER_EMAIL", "agent@goclaw.local"),
		GitHubToken:             os.Getenv("GOCLAW_GITHUB_TOKEN"),
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// expandHome replaces a leading "~" with home so GOCLAW_VAULT_DIR=~/Vault works.
func expandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}
