package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParseSpec(t *testing.T) {
	tests := []struct {
		spec             string
		url, subdir, ref string
		wantErr          bool
	}{
		{spec: "https://github.com/x/goclaw-roll", url: "https://github.com/x/goclaw-roll"},
		{spec: "https://github.com/x/goclaw-gmail#cmd/gmail", url: "https://github.com/x/goclaw-gmail", subdir: "cmd/gmail"},
		{spec: "https://github.com/x/goclaw-roll@v1.3.0", url: "https://github.com/x/goclaw-roll", ref: "v1.3.0"},
		{spec: "https://github.com/x/goclaw-gmail#cmd/gmail@v2.0.0", url: "https://github.com/x/goclaw-gmail", subdir: "cmd/gmail", ref: "v2.0.0"},
		{spec: "https://github.com/x/r@abc123", url: "https://github.com/x/r", ref: "abc123"},
		// Invalid pieces fail closed.
		{spec: "git@github.com:x/r.git", wantErr: true}, // scp-style url has no scheme prefix; '@' split then fails URL check
		{spec: "https://github.com/x/r#../escape", wantErr: true},
		{spec: "https://github.com/x/r@-rf", wantErr: true},
		{spec: "https://github.com/x/r@a b", wantErr: true},
		{spec: "ftp://github.com/x/r", wantErr: true},
	}
	for _, tt := range tests {
		url, subdir, ref, err := parseSpec(tt.spec)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseSpec(%q) = (%q,%q,%q), want error", tt.spec, url, subdir, ref)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSpec(%q) unexpected error: %v", tt.spec, err)
			continue
		}
		if url != tt.url || subdir != tt.subdir || ref != tt.ref {
			t.Errorf("parseSpec(%q) = (%q,%q,%q), want (%q,%q,%q)", tt.spec, url, subdir, ref, tt.url, tt.subdir, tt.ref)
		}
	}
}

func TestValidateRef(t *testing.T) {
	ok := []string{"", "v1.2.3", "main", "abc123def", "release-2026"}
	bad := []string{"-rf", "a b", "a;b", "../x", "v1..2", "a|b", "a$b", "a^b"}
	for _, r := range ok {
		if err := validateRef(r); err != nil {
			t.Errorf("validateRef(%q) = %v, want nil", r, err)
		}
	}
	for _, r := range bad {
		if err := validateRef(r); err == nil {
			t.Errorf("validateRef(%q) = nil, want error", r)
		}
	}
}

func TestParseSemverAndOrder(t *testing.T) {
	if _, err := parseSemver("1.2"); err == nil {
		t.Error("1.2 should not parse (needs three components)")
	}
	if _, err := parseSemver("1.2.0-rc1"); err == nil {
		t.Error("a pre-release tail must NOT parse (an update check must not offer a pre-release)")
	}
	a, err := parseSemver("v1.3.0")
	if err != nil {
		t.Fatalf("v1.3.0: %v", err)
	}
	b, _ := parseSemver("1.3.1")
	if !a.less(b) {
		t.Errorf("1.3.0 should be < 1.3.1")
	}
	if b.less(a) {
		t.Errorf("1.3.1 should NOT be < 1.3.0")
	}
	c, _ := parseSemver("2.0.0")
	if !b.less(c) {
		t.Errorf("major bump: 1.3.1 < 2.0.0")
	}
	if a.less(a) {
		t.Error("a version is not less than itself")
	}
}

// A plugin with no .source.json (installed before provenance tracking) must report
// Provenanceless without erroring AND without reaching the network, so a batch check can
// surface it. The installed dir exists but has no sidecar.
func TestCheckUpdate_Provenanceless(t *testing.T) {
	pluginsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(pluginsDir, "legacy"), 0o755); err != nil {
		t.Fatal(err)
	}
	in := NewInstaller(pluginsDir, "img", "podman")
	st, err := in.CheckUpdate(context.Background(), "legacy")
	if err != nil {
		t.Fatalf("CheckUpdate should not error on a provenanceless plugin: %v", err)
	}
	if !st.Provenanceless {
		t.Fatalf("want Provenanceless=true, got %+v", st)
	}
	if st.UpdateAvail {
		t.Fatal("a provenanceless plugin must never report an available update")
	}
}

// An invalid plugin name (path traversal) is rejected before any filesystem/network work.
func TestCheckUpdate_RejectsBadName(t *testing.T) {
	in := NewInstaller(t.TempDir(), "img", "podman")
	if _, err := in.CheckUpdate(context.Background(), "../etc"); err == nil {
		t.Fatal("expected an error for a traversal name")
	}
}

func TestLatestSemverTag(t *testing.T) {
	// Mixed input: branches, a pre-release, out-of-order tags. Highest STABLE v<semver> wins.
	tags := []string{"v1.0.0", "main", "v1.4.0", "v1.2.3", "v2.0.0-rc1", "release", "v1.10.0"}
	tag, ver, ok := latestSemverTag(tags)
	if !ok {
		t.Fatal("expected a winning tag")
	}
	// v1.10.0 > v1.4.0 (numeric, not lexical), and v2.0.0-rc1 is a pre-release (ignored).
	if tag != "v1.10.0" || ver.String() != "1.10.0" {
		t.Fatalf("latestSemverTag = (%q,%s), want (v1.10.0,1.10.0)", tag, ver)
	}

	// No qualifying tags.
	if _, _, ok := latestSemverTag([]string{"main", "dev", "v2-beta"}); ok {
		t.Error("no stable v<semver> tags should yield ok=false")
	}
}
