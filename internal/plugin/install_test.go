package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeOut stages a build-container output dir (binary + plugin.yml + .commit) the way
// the install sandbox would, so acceptArtifact can be tested without a real build.
func writeFakeOut(t *testing.T, name, version, commit string) string {
	t.Helper()
	out := t.TempDir()
	manifest := "name: " + name + "\nkind: tool\nexec: " + name + "\nversion: \"" + version + "\"\n"
	if err := os.WriteFile(filepath.Join(out, "plugin.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, name), []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, ".commit"), []byte(commit+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

// acceptArtifact writes the provenance sidecar (git_url/subdir/commit/version/installed_at)
// into the installed plugin dir, ReadSource reads it back, and an install-log line is
// appended. This is the core of the provenance + logging feature (docs/plugin-updates.md).
func TestAcceptArtifact_WritesSourceSidecarAndLog(t *testing.T) {
	// Pin the timestamp so the assertion is exact.
	oldNow := nowRFC3339
	nowRFC3339 = func() string { return "2026-06-07T08:00:00-07:00" }
	t.Cleanup(func() { nowRFC3339 = oldNow })

	pluginsDir := t.TempDir()
	in := NewInstaller(pluginsDir, "img", "podman")
	out := writeFakeOut(t, "gmail", "1.4.0", "abc123def456")

	res, err := in.acceptArtifact(out, "https://github.com/shindakun/goclaw-gmail", "cmd/gmail")
	if err != nil {
		t.Fatalf("acceptArtifact: %v", err)
	}

	// The result carries the provenance.
	if res.Source.GitURL != "https://github.com/shindakun/goclaw-gmail" ||
		res.Source.Subdir != "cmd/gmail" || res.Source.Commit != "abc123def456" ||
		res.Source.Version != "1.4.0" || res.Source.InstalledAt != "2026-06-07T08:00:00-07:00" {
		t.Fatalf("result source = %+v", res.Source)
	}

	// The sidecar is on disk in the installed dir and ReadSource round-trips it.
	got, err := ReadSource(res.Dir)
	if err != nil {
		t.Fatalf("ReadSource: %v", err)
	}
	if got != res.Source {
		t.Fatalf("ReadSource = %+v, want %+v", got, res.Source)
	}

	// The sidecar is dot-prefixed so the runner's plugin scan ignores it (it skips dot
	// entries), and it lives alongside the binary + plugin.yml.
	if _, err := os.Stat(filepath.Join(res.Dir, sourceFileName)); err != nil {
		t.Fatalf("sidecar missing: %v", err)
	}

	// acceptArtifact alone does NOT log (Add does); but exercise the log path via appendLog
	// the way Add would, and confirm the install-log line is appended.
	in.appendLog(installEvent{At: nowRFC3339(), Action: "install", Name: res.Name,
		GitURL: res.Source.GitURL, Subdir: res.Source.Subdir, Commit: res.Source.Commit, Vers: res.Version})
	logBytes, err := os.ReadFile(filepath.Join(pluginsDir, installLogName))
	if err != nil {
		t.Fatalf("read install log: %v", err)
	}
	line := strings.TrimSpace(string(logBytes))
	for _, want := range []string{`"action":"install"`, `"name":"gmail"`, `"commit":"abc123def456"`, `"git_url":"https://github.com/shindakun/goclaw-gmail"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("install-log line %q missing %q", line, want)
		}
	}
}

// ReadSource returns an error (not a panic) when the sidecar is absent, e.g. a plugin
// installed before provenance tracking. Callers treat that as "provenance unknown".
func TestReadSource_AbsentSidecar(t *testing.T) {
	if _, err := ReadSource(t.TempDir()); err == nil {
		t.Fatal("expected an error reading a missing sidecar")
	}
}

func TestValidateSubdir(t *testing.T) {
	ok := []string{
		"",          // empty = repo root, allowed
		"cmd/gmail", // the monorepo case
		"cmd/gmail-tools",
		"plugins/roll",
		"a",
	}
	for _, s := range ok {
		if err := validateSubdir(s); err != nil {
			t.Errorf("validateSubdir(%q) = %v, want nil", s, err)
		}
	}

	bad := map[string]string{
		"absolute":         "/etc",
		"leading slash":    "/cmd/gmail",
		"trailing slash":   "cmd/gmail/",
		"parent traversal": "cmd/../../etc",
		"dotdot only":      "..",
		"dot segment":      "cmd/./gmail",
		"empty segment":    "cmd//gmail",
		"space":            "cmd/gma il",
		"semicolon inject": "cmd/gmail;rm -rf /",
		"dollar inject":    "cmd/$HOME",
		"backtick inject":  "cmd/`id`",
		"glob star":        "cmd/*",
		"tilde":            "~/cmd",
		"backslash":        "cmd\\gmail",
	}
	for name, s := range bad {
		if err := validateSubdir(s); err == nil {
			t.Errorf("%s: validateSubdir(%q) = nil, want rejection", name, s)
		}
	}
}

// The spec "<url>#<subdir>" splits as expected, and a plain URL has no subdir. (This
// mirrors what Add does before validating each part.)
func TestSpecSplit(t *testing.T) {
	cases := []struct{ spec, url, sub string }{
		{"https://github.com/x/roll", "https://github.com/x/roll", ""},
		{"https://github.com/x/goclaw-gmail#cmd/gmail", "https://github.com/x/goclaw-gmail", "cmd/gmail"},
		{"https://github.com/x/goclaw-gmail#cmd/gmail-tools", "https://github.com/x/goclaw-gmail", "cmd/gmail-tools"},
	}
	for _, c := range cases {
		url, sub, _ := strings.Cut(c.spec, "#")
		if url != c.url || sub != c.sub {
			t.Errorf("split %q = (%q, %q), want (%q, %q)", c.spec, url, sub, c.url, c.sub)
		}
		// Both parts must validate for a well-formed monorepo spec.
		if err := validateGitURL(url); err != nil {
			t.Errorf("validateGitURL(%q): %v", url, err)
		}
		if err := validateSubdir(sub); err != nil {
			t.Errorf("validateSubdir(%q): %v", sub, err)
		}
	}
}
