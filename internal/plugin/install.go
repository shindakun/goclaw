package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// sourceFileName is the per-plugin provenance sidecar, written into the plugin's installed
// dir. Dot-prefixed so the runner's plugin watch (which skips hidden dirs/files) ignores it.
// It is the SINGLE source of truth for "where did this plugin come from" (no DB); it travels
// with the plugin dir and is host-owned + read-only in the container, so the container
// cannot rewrite it. See docs/plugin-updates.md.
const sourceFileName = ".source.json"

// Source is the provenance of an installed plugin: the git origin and the exact commit the
// installed binary was built from, plus the manifest version and when it was installed. It
// is the input an update check compares against upstream.
type Source struct {
	GitURL      string `json:"git_url"`
	Subdir      string `json:"subdir,omitempty"` // monorepo subdir, or "" for a root plugin
	Commit      string `json:"commit"`           // the pinned source commit the build came from
	Version     string `json:"version"`          // plugin.yml version at install time
	InstalledAt string `json:"installed_at"`     // RFC3339, host local time
}

// installLogName is the append-only audit log of install/update/remove events, one JSON
// line per event, in the plugins dir. Dot-prefixed so the runner's watch ignores it. This
// is the HISTORY store (the sidecar holds only current state); keeping it as a log, not DB
// rows, preserves the single-source-of-truth property. See docs/plugin-updates.md.
const installLogName = ".install-log.jsonl"

// installEvent is one line in the install log.
type installEvent struct {
	At     string `json:"at"`     // RFC3339
	Action string `json:"action"` // "install" | "remove"
	Name   string `json:"name"`
	GitURL string `json:"git_url,omitempty"`
	Subdir string `json:"subdir,omitempty"`
	Commit string `json:"commit,omitempty"`
	Vers   string `json:"version,omitempty"`
}

// Installer installs plugins by building them INSIDE a throwaway container, so
// untrusted plugin source is cloned, scanned, and compiled in the sandbox and
// never touches the host. Only the built artifact (a Linux binary + its
// plugin.yml) is copied out, into the host's plugins directory, which is mounted
// into the agent container where the runner discovers it (via fsnotify) and
// launches it. The host never clones, scans, builds, or executes the plugin.
type Installer struct {
	pluginsDir string // host plugins dir: ONLY finished, installed plugins live here
	stagingDir string // host dir the container hands the finished artifact back to
	image      string // OCI image with git + the Go toolchain (the runner image)
	podmanBin  string // podman binary
}

// NewInstaller builds an Installer. image must contain git and a Go toolchain
// (goclaw-claude:latest does); pluginsDir is the host plugins directory. The
// container hands its output back via a sibling `<pluginsDir>-staging` dir so the
// plugins dir (which the runner watches) only ever contains finished plugins. The
// actual build runs in the container (in /work), not in this dir.
func NewInstaller(pluginsDir, image, podmanBin string) *Installer {
	if podmanBin == "" {
		podmanBin = "podman"
	}
	return &Installer{
		pluginsDir: pluginsDir,
		stagingDir: pluginsDir + "-staging",
		image:      image,
		podmanBin:  podmanBin,
	}
}

// InstallResult reports what an install produced.
type InstallResult struct {
	Name    string // plugin name (from the built plugin.yml)
	Command string // slash command it registers ("" = none)
	Version string
	Commit  string // pinned source commit the build came from
	Dir     string // host dir the plugin was staged into (data/plugins/<name>)
	Source  Source // the provenance written to the sidecar
}

// Add clones a PUBLIC git repo, scans it, and builds it, all inside a throwaway
// container, then stages the verified artifact into the host plugins dir. It does
// NOT involve the credential proxy: a bare public clone needs no auth. Private
// repos are out of scope for now and fail with a clear message (see installScript).
// The spec is "<git-url>" for a one-plugin repo (plugin.yml at the root, the goclaw-roll
// layout), or "<git-url>#<subdir>" to select ONE plugin inside a monorepo (e.g.
// "...goclaw-gmail#cmd/gmail"), where <subdir> holds that plugin's plugin.yml. The whole
// repo is still scanned for red flags; only the manifest read + build come from <subdir>.
func (in *Installer) Add(ctx context.Context, spec string) (*InstallResult, error) {
	gitURL, subdir, _ := strings.Cut(spec, "#")
	if err := validateGitURL(gitURL); err != nil {
		return nil, err
	}
	if err := validateSubdir(subdir); err != nil {
		return nil, err
	}

	// A per-install staging dir the container hands its output back through (the
	// /out mount). It lives in the STAGING dir, NOT the plugins dir: the plugins dir
	// is watched by the runner and must contain only finished plugins. The actual
	// build runs in the container (in /work); this dir just receives the result. It
	// lives under the data dir (a path podman can bind-mount), unlike the OS temp dir
	// which may be a per-process path podman cannot see. The container writes only
	// the verified binary + plugin.yml + the pinned commit here; nothing else from
	// the untrusted source reaches the host.
	if err := os.MkdirAll(in.stagingDir, 0o755); err != nil {
		return nil, fmt.Errorf("install: create staging dir: %w", err)
	}
	out, err := os.MkdirTemp(in.stagingDir, "out-*")
	if err != nil {
		return nil, fmt.Errorf("install: staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(out) }()
	// The -v source MUST be absolute: podman treats a relative source as a named
	// VOLUME (and a path with '/' is an invalid volume name), which is the
	// "creating named volume" error. pluginsDir may be relative (e.g. data/plugins).
	outAbs, err := filepath.Abs(out)
	if err != nil {
		return nil, fmt.Errorf("install: resolve staging dir: %w", err)
	}

	// Run the build entirely in the sandbox: clone -> scan -> build -> stage to
	// /out. The container is rootless, non-root, and removed on exit; only /out is
	// shared back to the host.
	args := []string{
		"run", "--rm",
		"--user", "1000:1000",
		"-v", outAbs + ":/out:Z",
		"-e", "PLUGIN_GIT_URL=" + gitURL,
		"-e", "PLUGIN_SUBDIR=" + subdir,
		"--entrypoint", "sh",
		in.image, "-c", installScript,
	}
	cmd := exec.CommandContext(ctx, in.podmanBin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("install: build failed: %s", msg)
	}

	// The build container wrote the artifact to /out. Read what it staged.
	res, err := in.acceptArtifact(out, gitURL, subdir)
	if err != nil {
		return nil, err
	}
	in.appendLog(installEvent{
		At: nowRFC3339(), Action: "install", Name: res.Name,
		GitURL: res.Source.GitURL, Subdir: res.Source.Subdir,
		Commit: res.Source.Commit, Vers: res.Version,
	})
	return res, nil
}

// acceptArtifact validates the build container's output and moves it into the host plugins
// dir atomically, writing the provenance sidecar (gitURL/subdir/commit/version/installed_at)
// alongside the binary + plugin.yml so all three land together on the rename.
func (in *Installer) acceptArtifact(out, gitURL, subdir string) (*InstallResult, error) {
	man, err := LoadManifest(out) // the build wrote plugin.yml here
	if err != nil {
		return nil, fmt.Errorf("install: built artifact has no valid plugin.yml: %w", err)
	}
	binPath := filepath.Join(out, man.Exec)
	if _, err := os.Stat(binPath); err != nil {
		return nil, fmt.Errorf("install: built binary %q missing: %w", man.Exec, err)
	}
	commit := strings.TrimSpace(readFileOr(filepath.Join(out, ".commit"), ""))

	src := Source{
		GitURL:      gitURL,
		Subdir:      subdir,
		Commit:      commit,
		Version:     man.Version,
		InstalledAt: nowRFC3339(),
	}
	srcJSON, err := json.MarshalIndent(src, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("install: marshal source: %w", err)
	}

	dest := filepath.Join(in.pluginsDir, man.Name)
	if err := os.MkdirAll(in.pluginsDir, 0o755); err != nil {
		return nil, fmt.Errorf("install: create plugins dir: %w", err)
	}
	// Stage atomically: write to a HIDDEN sibling dir, then rename it onto the dest.
	// The dir is dot-prefixed so the runner's watch (which skips hidden dirs) never
	// loads a half-staged plugin; only the final rename (a complete, non-hidden dir
	// appearing) triggers a load. The sidecar is written INTO the staging dir so it is
	// part of the same atomic rename, never a separate write that could be seen alone.
	staging := filepath.Join(in.pluginsDir, "."+man.Name+".installing")
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return nil, fmt.Errorf("install: staging dir: %w", err)
	}
	if err := copyFile(binPath, filepath.Join(staging, man.Exec), 0o755); err != nil {
		return nil, err
	}
	if err := copyFile(filepath.Join(out, "plugin.yml"), filepath.Join(staging, "plugin.yml"), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(staging, sourceFileName), srcJSON, 0o644); err != nil {
		return nil, fmt.Errorf("install: write source sidecar: %w", err)
	}
	_ = os.RemoveAll(dest)
	if err := os.Rename(staging, dest); err != nil {
		return nil, fmt.Errorf("install: finalize %q: %w", dest, err)
	}

	return &InstallResult{
		Name:    man.Name,
		Command: man.Command,
		Version: man.Version,
		Commit:  commit,
		Dir:     dest,
		Source:  src,
	}, nil
}

// Remove deletes an installed plugin's directory. The runner's plugin reconcile (watch +
// poll) reacts to the removal and stops the plugin. Returns whether anything was removed.
func (in *Installer) Remove(name string) (bool, error) {
	name = sanitizeName(name)
	if name == "" {
		return false, fmt.Errorf("install: invalid plugin name")
	}
	dir := filepath.Join(in.pluginsDir, name)
	if _, err := os.Stat(dir); err != nil {
		return false, nil // not installed
	}
	// Read provenance before deleting, so the remove event can record what was removed.
	src, _ := ReadSource(dir)
	if err := os.RemoveAll(dir); err != nil {
		return false, fmt.Errorf("install: remove %q: %w", dir, err)
	}
	in.appendLog(installEvent{
		At: nowRFC3339(), Action: "remove", Name: name,
		GitURL: src.GitURL, Subdir: src.Subdir, Commit: src.Commit, Vers: src.Version,
	})
	return true, nil
}

// ReadSource reads the provenance sidecar from an installed plugin's dir. Returns a zero
// Source and an error if the sidecar is absent (e.g. a plugin installed before provenance
// tracking) or unreadable; callers treat that as "provenance unknown", not fatal.
func ReadSource(pluginDir string) (Source, error) {
	b, err := os.ReadFile(filepath.Join(pluginDir, sourceFileName))
	if err != nil {
		return Source{}, err
	}
	var s Source
	if err := json.Unmarshal(b, &s); err != nil {
		return Source{}, fmt.Errorf("install: parse source sidecar: %w", err)
	}
	return s, nil
}

// appendLog appends one event to the install log (.install-log.jsonl in the plugins dir).
// Best-effort: a logging failure must never fail an install/remove, so errors are swallowed
// (the structured host log line from the router is the primary signal; this is the durable
// audit trail).
func (in *Installer) appendLog(ev installEvent) {
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if err := os.MkdirAll(in.pluginsDir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(in.pluginsDir, installLogName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(line, '\n'))
}

// nowRFC3339 is the install timestamp source, overridable in tests.
var nowRFC3339 = func() string { return time.Now().Format(time.RFC3339) }

// List returns the installed plugins' manifests, sorted by name.
func (in *Installer) List() ([]Manifest, error) {
	entries, err := os.ReadDir(in.pluginsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Manifest
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		man, err := LoadManifest(filepath.Join(in.pluginsDir, e.Name()))
		if err != nil {
			continue // skip a half-installed or malformed dir
		}
		out = append(out, man)
	}
	return out, nil
}

// secretEnvScanPattern is the extended-regex (egrep) alternation the install scan
// uses to reject plugin source that references goclaw's host/agent credential env var
// names, or the distinctive fragments those names split into (to catch a
// "ANTHROPIC" + "_API_KEY" style concatenation). It is a BEST-EFFORT DETERRENT, not a
// guarantee: a runtime-assembled name (mid-word splits, base64, a fetched value)
// evades any static grep. The real protections are the env allowlist (InjectEnv) and
// the credential proxy. Common tokens (API/KEY/TOKEN alone) are deliberately excluded
// to avoid false-positiving a plugin's own config; os.Environ() is not matched.
// Exported via this constant so secretEnvScanPattern_test can verify catch/no-catch
// against fixtures without the regex drifting from the script.
const secretEnvScanPattern = `ANTHROPIC_API_KEY|ANTHROPIC|GH_TOKEN|GOCLAW_GITHUB_TOKEN|GITHUB_TOKEN|CLAUDE_CODE_OAUTH_TOKEN|_OAUTH_TOKEN|GOCLAW_SECRET_ENCRYPTION_KEY|SECRET_ENCRYPTION`

// installScript runs inside the build container. It clones (public, no auth),
// scans the source for high-risk build-time and runtime red flags, builds a
// pure-Go Linux binary, verifies it imports goclawkit, and stages the artifact +
// pinned commit into /out. Any failure exits non-zero and aborts the install.
//
// SECURITY: every step here runs in the sandbox (rootless, non-root, /out is the
// only host-shared path). The host never sees or runs the untrusted source.
//
// installScript is composed (not a plain const) so the secret-scan regex is the single
// source of truth shared with the test (secretEnvScanPattern).
var installScript = `
set -eu
SRC=/work/plugin-src
rm -rf "$SRC"

# Bare PUBLIC clone, shallow. No credentials: private repos are out of scope.
# Disable any interactive/credential prompt so a private URL fails fast instead
# of hanging.
export GIT_TERMINAL_PROMPT=0
if ! git clone --depth 1 "$PLUGIN_GIT_URL" "$SRC" 2>/tmp/clone.err; then
  echo "clone failed (private repos are not supported yet):" >&2
  cat /tmp/clone.err >&2
  exit 1
fi

cd "$SRC"
COMMIT=$(git rev-parse HEAD)

# --- Red-flag scan (build-time and runtime). Reject the obviously dangerous. ---
# cgo: import "C" lets arbitrary C compile/link. go:generate runs arbitrary
# commands at build. replace directives can pull code from anywhere. These are
# the build-time execution vectors we refuse.
if grep -rEn 'import +"C"' --include='*.go' . >/dev/null 2>&1; then
  echo "rejected: plugin uses cgo (import \"C\")" >&2; exit 2
fi
if grep -rEn '^//go:generate' --include='*.go' . >/dev/null 2>&1; then
  echo "rejected: plugin uses //go:generate (runs commands at build)" >&2; exit 2
fi
if grep -En '^[[:space:]]*replace[[:space:]]' go.mod >/dev/null 2>&1; then
  echo "rejected: go.mod has a replace directive" >&2; exit 2
fi
# Must depend on the plugin SDK (a goclaw plugin imports goclawkit).
if ! grep -q 'github.com/shindakun/goclawkit' go.mod; then
  echo "rejected: not a goclaw plugin (does not require goclawkit)" >&2; exit 2
fi

# --- Host-secret read scan (DEFENSE IN DEPTH, best-effort, EVADABLE). ---
# A plugin never legitimately needs to read goclaw's host/agent credential env
# vars. Reject a plugin whose source names them, or names the distinctive
# fragments those names split into (to catch a literal "ANT"+"HROPIC_API_KEY"
# style concatenation). This is a deterrent against a naive hostile plugin and a
# guard against an accidental leak, NOT a guarantee: a determined plugin can
# assemble the var name at runtime (string math, base64, a fetched constant) and
# slip past any static grep. The REAL protections are the env allowlist (the
# plugin is never handed these vars; see Manifest.InjectEnv) and the credential
# proxy (the container holds only placeholders). Because plugins are open-source
# code the operator chooses to install, the final responsibility is the operator's:
# install plugins you trust. See docs/security.md.
#
# Fragments are chosen to be distinctive enough that a benign plugin will not
# contain them; common tokens like API/KEY/TOKEN alone are deliberately NOT matched
# (they false-positive a plugin's own config). os.Environ() is deliberately NOT
# matched (too many benign uses).
if grep -rEn '` + secretEnvScanPattern + `' --include='*.go' . >/dev/null 2>&1; then
  echo "rejected: plugin source references a host secret env var (e.g. ANTHROPIC_API_KEY / GH_TOKEN). A plugin has no reason to read goclaw's credentials." >&2; exit 2
fi

# --- Locate the plugin dir. PLUGIN_SUBDIR selects ONE plugin inside a monorepo
# (e.g. cmd/gmail); empty means the plugin lives at the repo root (the one-plugin
# layout). The scan above intentionally covered the WHOLE repo ($SRC), so a malicious
# file in a shared dir (e.g. internal/) is caught even when we build only a subdir.
# PLUGIN_SUBDIR is validated host-side (no .. , no absolute, no metacharacters) and the
# resolved path must stay inside $SRC. ---
PLUGIN_DIR="$SRC"
if [ -n "${PLUGIN_SUBDIR:-}" ]; then
  PLUGIN_DIR="$SRC/$PLUGIN_SUBDIR"
  # Defense in depth: confirm the resolved dir is really inside the clone.
  case "$(cd "$PLUGIN_DIR" 2>/dev/null && pwd -P)" in
    "$SRC"|"$SRC"/*) : ;;
    *) echo "rejected: plugin subdir $PLUGIN_SUBDIR escapes the repo" >&2; exit 2 ;;
  esac
fi
[ -f "$PLUGIN_DIR/plugin.yml" ] || { echo "rejected: no plugin.yml in ${PLUGIN_SUBDIR:-<repo root>}" >&2; exit 2; }
cd "$PLUGIN_DIR"

# --- Build: pure-Go, Linux, the container's arch. CGO off both enforces purity
# and matches how the plugin must run. Build the plugin dir's package (.). ---
EXEC=$(sed -n 's/^exec:[[:space:]]*//p' plugin.yml | head -n1 | tr -d '"' | tr -d "'" | awk '{print $1}')
[ -n "$EXEC" ] || { echo "rejected: plugin.yml has no exec field" >&2; exit 2; }
CGO_ENABLED=0 GOOS=linux GOFLAGS=-mod=mod go build -trimpath -o "/out/$EXEC" . 2>/tmp/build.err || {
  echo "build failed:" >&2; cat /tmp/build.err >&2; exit 3;
}

# Stage the manifest and the pinned commit beside the binary.
cp plugin.yml /out/plugin.yml
printf '%s' "$COMMIT" > /out/.commit
echo "built $EXEC at $COMMIT"
`

// validateGitURL accepts only a plausible public http(s) or git URL and rejects
// anything that looks like a shell-injection or a local path.
func validateGitURL(u string) error {
	u = strings.TrimSpace(u)
	if u == "" {
		return fmt.Errorf("install: empty git url")
	}
	if strings.ContainsAny(u, " \t\n;|&$`\\\"'") {
		return fmt.Errorf("install: git url contains illegal characters")
	}
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "git://") {
		return fmt.Errorf("install: git url must be http(s) or git (got %q)", u)
	}
	return nil
}

// validateSubdir checks the optional in-repo plugin subdir (the part after '#' in a
// "<url>#<subdir>" spec). Empty is fine (the plugin is at the repo root). A non-empty
// subdir must be a CLEAN, relative, forward-slash path that cannot escape the repo: no
// absolute path, no "." or ".." component, no leading/trailing slash, no shell
// metacharacters or backslashes. The value reaches the build container only as an env
// var (PLUGIN_SUBDIR), never interpolated into the script, and the script also verifies
// the resolved path stays inside the clone, this is the host-side first line.
func validateSubdir(sub string) error {
	if sub == "" {
		return nil
	}
	if strings.ContainsAny(sub, " \t\n;|&$`\\\"'*?~") {
		return fmt.Errorf("install: plugin subdir contains illegal characters")
	}
	if strings.HasPrefix(sub, "/") || strings.HasSuffix(sub, "/") {
		return fmt.Errorf("install: plugin subdir must be relative without a trailing slash (got %q)", sub)
	}
	for _, seg := range strings.Split(sub, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("install: plugin subdir has an empty or traversal segment (got %q)", sub)
		}
	}
	return nil
}

// sanitizeName guards against path traversal in a plugin name from chat input.
func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, "/\\.") {
		return ""
	}
	return name
}

func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("install: read %q: %w", src, err)
	}
	if err := os.WriteFile(dst, data, mode); err != nil {
		return fmt.Errorf("install: write %q: %w", dst, err)
	}
	return nil
}

func readFileOr(path, fallback string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return fallback
	}
	return string(b)
}

// PluginsDir returns the host plugins directory artifacts are staged into.
func (in *Installer) PluginsDir() string { return in.pluginsDir }
