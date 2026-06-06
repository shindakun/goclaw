package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadManifest_Valid(t *testing.T) {
	dir := writeManifest(t, "name: roll\nkind: tool\nversion: \"1.0.0\"\nexec: roll\ncommand: roll\n")
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	if m.Name != "roll" || m.Exec != "roll" || m.ExecPath() != filepath.Join(dir, "roll") {
		t.Fatalf("unexpected manifest: %+v", m)
	}
}

func TestLoadManifest_RejectsBadExec(t *testing.T) {
	cases := map[string]string{
		"path traversal": "name: x\nkind: tool\nexec: ../evil\n",
		"absolute path":  "name: x\nkind: tool\nexec: /bin/sh\n",
		"space in name":  "name: x\nkind: tool\nexec: my plugin\n",
		"missing exec":   "name: x\nkind: tool\n",
		"unknown kind":   "name: x\nkind: gadget\nexec: x\n",
		"missing name":   "kind: tool\nexec: x\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadManifest(writeManifest(t, body)); err == nil {
				t.Fatalf("expected rejection for %s", name)
			}
		})
	}
}

func TestManifest_InjectEnv(t *testing.T) {
	m := Manifest{Env: []string{"IRC_SERVER", "IRC_NICK", "IRC_ABSENT"}}
	lookup := func(k string) (string, bool) {
		switch k {
		case "IRC_SERVER":
			return "irc.example.net:6697", true
		case "IRC_NICK":
			return "bot", true
		default:
			return "", false // IRC_ABSENT is not set
		}
	}
	base := []string{"PATH=/usr/bin", "HOME=/root"}
	got := m.InjectEnv(base, lookup)

	// Base is preserved verbatim and comes first.
	if len(got) < len(base) || got[0] != "PATH=/usr/bin" || got[1] != "HOME=/root" {
		t.Fatalf("base not preserved: %v", got)
	}
	// Present names are appended as KEY=VALUE.
	has := func(kv string) bool {
		for _, e := range got {
			if e == kv {
				return true
			}
		}
		return false
	}
	if !has("IRC_SERVER=irc.example.net:6697") || !has("IRC_NICK=bot") {
		t.Fatalf("declared env not injected: %v", got)
	}
	// An absent name is SKIPPED, not appended empty (so the plugin keeps its default).
	for _, e := range got {
		if e == "IRC_ABSENT=" || e == "IRC_ABSENT" {
			t.Fatalf("absent env was injected empty: %v", got)
		}
	}
	if len(got) != len(base)+2 {
		t.Fatalf("expected base+2 entries, got %d: %v", len(got), got)
	}
}

// MinimalEnvBase must carry the proxy-routing vars (so a target-mode plugin can reach the
// credential proxy) but NOT arbitrary host env, least of all secrets. This is the fix for
// the live bug where the gmail plugin connected directly to Gmail (no proxy) and got 401s
// because HTTPS_PROXY/SSL_CERT_FILE were filtered out of its environment.
func TestMinimalEnvBase_CarriesProxyRoutingNotSecrets(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://host.docker.internal:18080")
	t.Setenv("https_proxy", "http://host.docker.internal:18080")
	t.Setenv("NO_PROXY", "host.docker.internal,localhost")
	t.Setenv("SSL_CERT_FILE", "/etc/goclaw/proxy-ca.pem")
	// A host secret that must NEVER appear in a plugin's base env.
	t.Setenv("GOCLAW_ANTHROPIC_API_KEY", "sk-ant-should-not-leak")
	t.Setenv("TELEGRAM_BOT_TOKEN", "telegram-should-not-leak")

	base := MinimalEnvBase()
	has := func(prefix string) bool {
		for _, e := range base {
			if strings.HasPrefix(e, prefix) {
				return true
			}
		}
		return false
	}

	// Proxy-routing vars are present (these MAKE the proxy injection work).
	for _, want := range []string{"HTTPS_PROXY=", "https_proxy=", "NO_PROXY=", "SSL_CERT_FILE="} {
		if !has(want) {
			t.Errorf("MinimalEnvBase missing proxy-routing var %q: %v", want, base)
		}
	}
	// PATH is still carried.
	if !has("PATH=") {
		t.Errorf("MinimalEnvBase dropped PATH: %v", base)
	}
	// Secrets must NOT be carried (the whole point of not using os.Environ()).
	for _, secret := range []string{"GOCLAW_ANTHROPIC_API_KEY", "TELEGRAM_BOT_TOKEN", "sk-ant-should-not-leak", "telegram-should-not-leak"} {
		if has(secret) {
			t.Fatalf("MinimalEnvBase LEAKED a secret %q into the plugin base env: %v", secret, base)
		}
	}
}

func TestLoadManifest_ChannelKind(t *testing.T) {
	// A channel manifest (no command) is accepted.
	dir := writeManifest(t, "name: irc\nkind: channel\nversion: \"1.0.0\"\nexec: irc\nenv:\n  - IRC_SERVER\n")
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("valid channel manifest rejected: %v", err)
	}
	if m.Kind != "channel" || m.Name != "irc" {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	if len(m.Env) != 1 || m.Env[0] != "IRC_SERVER" {
		t.Fatalf("env not parsed: %+v", m.Env)
	}

	// A channel that declares a command is rejected (a channel is not a slash command).
	bad := writeManifest(t, "name: irc\nkind: channel\nexec: irc\ncommand: irc\n")
	if _, err := LoadManifest(bad); err == nil {
		t.Fatal("channel with a command should be rejected")
	}
}
