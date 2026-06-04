package config

import (
	"path/filepath"
	"testing"
)

func TestExpandHome(t *testing.T) {
	home := "/home/steve"
	cases := []struct {
		in, want string
	}{
		{"~", home},
		{"~/Vault", filepath.Join(home, "Vault")},
		{"~/a/b/c", filepath.Join(home, "a/b/c")},
		{"/abs/path", "/abs/path"},         // absolute untouched
		{"relative/path", "relative/path"}, // relative untouched
		{"", ""},                           // empty untouched
		{"~user", "~user"},                 // only bare ~ or ~/ expand, not ~user
	}
	for _, c := range cases {
		if got := expandHome(c.in, home); got != c.want {
			t.Errorf("expandHome(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestGetenv(t *testing.T) {
	t.Setenv("GOCLAW_TEST_SET", "value")
	if got := getenv("GOCLAW_TEST_SET", "fallback"); got != "value" {
		t.Errorf("set var = %q, want value", got)
	}
	t.Setenv("GOCLAW_TEST_EMPTY", "")
	if got := getenv("GOCLAW_TEST_EMPTY", "fallback"); got != "fallback" {
		t.Errorf("empty var should use fallback, got %q", got)
	}
	if got := getenv("GOCLAW_TEST_UNSET_XYZ", "fallback"); got != "fallback" {
		t.Errorf("unset var should use fallback, got %q", got)
	}
}

// Load applies defaults and reads env. Run it with a clean-ish env to confirm the
// documented defaults land. t.Setenv isolates these to the test process.
func TestLoad_Defaults(t *testing.T) {
	// Clear the channel/owner vars so we exercise the empty path; set HOME so
	// UserHomeDir is deterministic across platforms is not guaranteed, so we only
	// assert defaults that do not depend on home.
	t.Setenv("GOCLAW_DATA_DIR", "")
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("GOCLAW_DISCORD_TOKEN", "")
	t.Setenv("GOCLAW_LAUNCH_RUNNER", "")
	t.Setenv("GOCLAW_AUTO_WIRE_OWNER", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataDir != "data" {
		t.Errorf("DataDir default = %q, want data", cfg.DataDir)
	}
	if cfg.CentralDBPath != filepath.Join("data", "goclaw.db") {
		t.Errorf("CentralDBPath = %q", cfg.CentralDBPath)
	}
	if cfg.PodmanBin != "podman" {
		t.Errorf("PodmanBin default = %q", cfg.PodmanBin)
	}
	if cfg.RunnerImage != "goclaw-claude:latest" {
		t.Errorf("RunnerImage default = %q", cfg.RunnerImage)
	}
	if cfg.CredProxyPort != "18080" {
		t.Errorf("CredProxyPort default = %q", cfg.CredProxyPort)
	}
	if cfg.GitUserName != "goclaw agent" || cfg.GitUserEmail != "agent@goclaw.local" {
		t.Errorf("git identity defaults wrong: %q / %q", cfg.GitUserName, cfg.GitUserEmail)
	}
	if cfg.LaunchRunner || cfg.AutoWireOwner {
		t.Error("boolean flags should default false")
	}
}

func TestLoad_BooleanFlagsParseOne(t *testing.T) {
	t.Setenv("GOCLAW_LAUNCH_RUNNER", "1")
	t.Setenv("GOCLAW_AUTO_WIRE_OWNER", "1")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.LaunchRunner || !cfg.AutoWireOwner {
		t.Error("expected boolean flags true when set to 1")
	}
	// Any value other than exactly "1" is false.
	t.Setenv("GOCLAW_LAUNCH_RUNNER", "true")
	cfg, _ = Load()
	if cfg.LaunchRunner {
		t.Error(`LaunchRunner should be false unless value is exactly "1"`)
	}
}
