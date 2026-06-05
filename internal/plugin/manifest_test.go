package plugin

import (
	"os"
	"path/filepath"
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
