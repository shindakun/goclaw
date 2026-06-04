package plugin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Installer installs plugins by building them INSIDE a throwaway container, so
// untrusted plugin source is cloned, scanned, and compiled in the sandbox and
// never touches the host. Only the built artifact (a Linux binary + its
// plugin.yml) is copied out, into the host's plugins directory, which is mounted
// into the agent container where the runner discovers it (via fsnotify) and
// launches it. The host never clones, scans, builds, or executes the plugin.
type Installer struct {
	pluginsDir string // host data/plugins dir (staged artifacts land here)
	image      string // OCI image with git + the Go toolchain (the runner image)
	podmanBin  string // podman binary
}

// NewInstaller builds an Installer. image must contain git and a Go toolchain
// (goclaw-claude:latest does); pluginsDir is the host data/plugins directory.
func NewInstaller(pluginsDir, image, podmanBin string) *Installer {
	if podmanBin == "" {
		podmanBin = "podman"
	}
	return &Installer{pluginsDir: pluginsDir, image: image, podmanBin: podmanBin}
}

// InstallResult reports what an install produced.
type InstallResult struct {
	Name    string // plugin name (from the built plugin.yml)
	Command string // slash command it registers ("" = none)
	Version string
	Commit  string // pinned source commit the build came from
	Dir     string // host dir the plugin was staged into (data/plugins/<name>)
}

// Add clones a PUBLIC git repo, scans it, and builds it, all inside a throwaway
// container, then stages the verified artifact into the host plugins dir. It does
// NOT involve the credential proxy: a bare public clone needs no auth. Private
// repos are out of scope for now and fail with a clear message (see installScript).
func (in *Installer) Add(ctx context.Context, gitURL string) (*InstallResult, error) {
	if err := validateGitURL(gitURL); err != nil {
		return nil, err
	}

	// A per-install staging dir for the build container's /out mount. It must live
	// somewhere podman can bind-mount, so it sits NEXT TO the plugins dir (under the
	// data dir), NOT in the OS temp dir, which may be a per-process path podman
	// cannot see. The build container writes only the verified binary + plugin.yml +
	// a meta file here; nothing else from the untrusted source reaches the host.
	if err := os.MkdirAll(in.pluginsDir, 0o755); err != nil {
		return nil, fmt.Errorf("install: create plugins dir: %w", err)
	}
	out, err := os.MkdirTemp(in.pluginsDir, ".build-*")
	if err != nil {
		return nil, fmt.Errorf("install: staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(out) }()

	// Run the build entirely in the sandbox: clone -> scan -> build -> stage to
	// /out. The container is rootless, non-root, and removed on exit; only /out is
	// shared back to the host.
	args := []string{
		"run", "--rm",
		"--user", "1000:1000",
		"-v", out + ":/out:Z",
		"-e", "PLUGIN_GIT_URL=" + gitURL,
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
	res, err := in.acceptArtifact(out)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// acceptArtifact validates the build container's output and moves it into the
// host plugins dir atomically.
func (in *Installer) acceptArtifact(out string) (*InstallResult, error) {
	man, err := LoadManifest(out) // the build wrote plugin.yml here
	if err != nil {
		return nil, fmt.Errorf("install: built artifact has no valid plugin.yml: %w", err)
	}
	binPath := filepath.Join(out, man.Exec)
	if _, err := os.Stat(binPath); err != nil {
		return nil, fmt.Errorf("install: built binary %q missing: %w", man.Exec, err)
	}
	commit := strings.TrimSpace(readFileOr(filepath.Join(out, ".commit"), ""))

	dest := filepath.Join(in.pluginsDir, man.Name)
	if err := os.MkdirAll(in.pluginsDir, 0o755); err != nil {
		return nil, fmt.Errorf("install: create plugins dir: %w", err)
	}
	// Stage atomically: write to a sibling temp dir, then rename over the dest. The
	// rename is what the runner's fsnotify watch reacts to (a complete dir appears).
	staging := dest + ".installing"
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
	}, nil
}

// Remove deletes an installed plugin's directory. The runner's fsnotify watch
// reacts to the removal and stops the plugin. Returns whether anything was removed.
func (in *Installer) Remove(name string) (bool, error) {
	name = sanitizeName(name)
	if name == "" {
		return false, fmt.Errorf("install: invalid plugin name")
	}
	dir := filepath.Join(in.pluginsDir, name)
	if _, err := os.Stat(dir); err != nil {
		return false, nil // not installed
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, fmt.Errorf("install: remove %q: %w", dir, err)
	}
	return true, nil
}

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

// installScript runs inside the build container. It clones (public, no auth),
// scans the source for high-risk build-time and runtime red flags, builds a
// pure-Go Linux binary, verifies it imports goclawkit, and stages the artifact +
// pinned commit into /out. Any failure exits non-zero and aborts the install.
//
// SECURITY: every step here runs in the sandbox (rootless, non-root, /out is the
// only host-shared path). The host never sees or runs the untrusted source.
const installScript = `
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

# --- Build: pure-Go, Linux, the container's arch. CGO off both enforces purity
# and matches how the plugin must run. ---
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
