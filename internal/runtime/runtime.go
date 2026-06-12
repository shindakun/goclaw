// Package runtime manages per-agent-group container lifecycle on Podman
// (brief §6). v0 shells out to the `podman` CLI - simplest, easy to audit, and
// mirrors what the original container-runner.ts effectively did. Move to the
// Podman Go bindings later if stronger typing is wanted (brief §6.2).
//
// Security defaults baked in here (brief §6, §9):
//   - rootless, non-root in container (--user 1000:1000),
//   - an init process for PID-1 signal handling (--init),
//   - validated mounts with :Z relabeling (via internal/mounts),
//   - per-group runtime selection (crun default; gvisor/kata opt-in).
package runtime

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/shindakun/goclaw/internal/mounts"
)

// execCommand builds an *exec.Cmd. It is a package var so tests can substitute a
// fake that records argv / returns canned output without invoking podman.
var execCommand = exec.CommandContext

// Runtime selects the OCI runtime for a container (brief §6.4).
type Runtime string

const (
	RuntimeCrun   Runtime = "crun"  // default rootless runtime
	RuntimeGVisor Runtime = "runsc" // gVisor syscall-interception sandbox
	RuntimeKata   Runtime = "kata"  // Kata Containers micro-VM
)

// Spec describes a container to launch for one agent group.
type Spec struct {
	Name    string         // container name
	Image   string         // OCI image reference
	Runtime Runtime        // OCI runtime
	Mounts  []mounts.Mount // already validated by internal/mounts
	Env     map[string]string
}

// Manager drives Podman.
type Manager struct {
	podmanBin   string            // path/name of the podman binary
	image       string            // runner image to launch
	runtime     Runtime           // OCI runtime for launched containers
	allowlist   *mounts.Allowlist // validates extra group mounts (may be nil)
	env         map[string]string // env vars set on every launched container
	vaultDir    string            // host knowledge-vault dir, mounted at /vault (may be empty)
	caCertPath  string            // host path to the credential-proxy CA cert, mounted RO (may be empty)
	pluginsDir  string            // host plugins dir, mounted RO at /plugins (may be empty)
	chanSockDir string            // host channel-socket dir, mounted RW at /run/goclaw/channels (may be empty)

	// ensureMu serializes EnsureGroupRunner per agent group. The router (on a new
	// message) and the sweep (recovery) both call EnsureRunner concurrently; the
	// check-then-remove-then-run sequence must be atomic, or two callers can both
	// pass the "not running / wrong image" check and launch/replace at once -
	// briefly running TWO containers for one group, which duplicates and drops
	// replies (and cancels in-flight work). A per-group lock makes it atomic.
	muMapMu  sync.Mutex
	ensureMu map[int64]*sync.Mutex
}

// New constructs a Manager. binary defaults to "podman" and runtime to crun if
// empty. image is the runner image launched by EnsureRunner. allowlist (may be
// nil) validates a group's extra mounts; a nil allowlist permits no extras.
func New(binary, image string, rt Runtime, allowlist *mounts.Allowlist) *Manager {
	if binary == "" {
		binary = "podman"
	}
	if rt == "" {
		rt = RuntimeCrun
	}
	return &Manager{
		podmanBin: binary,
		image:     image,
		runtime:   rt,
		allowlist: allowlist,
		ensureMu:  make(map[int64]*sync.Mutex),
	}
}

// groupLock returns the per-agent-group ensure lock, creating it on first use.
func (m *Manager) groupLock(agentGroupID int64) *sync.Mutex {
	m.muMapMu.Lock()
	defer m.muMapMu.Unlock()
	mu, ok := m.ensureMu[agentGroupID]
	if !ok {
		mu = &sync.Mutex{}
		m.ensureMu[agentGroupID] = mu
	}
	return mu
}

// WithEnv sets environment variables passed into every launched runner
// container (e.g. ANTHROPIC_API_KEY for the Claude runner). Returns m for
// chaining. Empty values are dropped so an unset key doesn't blank the var.
func (m *Manager) WithEnv(env map[string]string) *Manager {
	clean := make(map[string]string, len(env))
	for k, v := range env {
		if v != "" {
			clean[k] = v
		}
	}
	m.env = clean
	return m
}

// WithVault sets the host knowledge-vault directory, mounted read-write at
// /vault in every launched runner container (brief §11). Empty disables it.
// Returns m for chaining.
func (m *Manager) WithVault(dir string) *Manager {
	m.vaultDir = dir
	return m
}

// WithCredCA sets the host path to the credential-proxy CA certificate, mounted
// read-only into the container so the agent's tools trust the proxy's
// intercepted TLS (brief §8). Empty disables it. Returns m for chaining.
func (m *Manager) WithCredCA(hostCAPath string) *Manager {
	m.caCertPath = hostCAPath
	return m
}

// WithPlugins sets the host directory of installed plugins, mounted READ-ONLY at
// /plugins in every runner container. Plugins are untrusted downloaded-and-compiled
// code; mounting them read-only into the agent's sandbox (rather than running them
// on the host) is the security boundary. The in-container runner discovers and
// launches them. Empty disables plugins. Returns m for chaining.
func (m *Manager) WithPlugins(dir string) *Manager {
	m.pluginsDir = dir
	return m
}

// WithChannelSockets sets the host directory holding per-channel Unix sockets, mounted
// READ-WRITE at /run/goclaw/channels in every runner container. The HOST listens on
// each <name>.sock (the trusted channel relay); the in-container runner DIALS it and
// bridges the channel plugin's stdio across, so the host drives a sandboxed channel
// plugin without ever running it. This is the ONE container mount the runner writes
// (a duplex socket), distinct from the read-only /plugins and the single-writer SQLite
// pair. Empty disables channel plugins. Returns m for chaining.
func (m *Manager) WithChannelSockets(dir string) *Manager {
	m.chanSockDir = dir
	return m
}

// EnsureRunner implements the RunnerEnsurer interface used by the router and
// sweep: it ensures a runner container is up for the given agent group,
// launching one (mounting groupDir at /sessions, plus any extra mounts that
// pass allowlist validation) if not. Idempotent - a running container is left
// alone. extra mounts that fail validation are skipped (fail closed) and logged
// by the caller via the returned-from-Validate errors collected here.
func (m *Manager) EnsureRunner(ctx context.Context, agentGroupID int64, groupDir string, extra ...mounts.Request) error {
	validated := m.validateExtra(extra)
	return m.EnsureGroupRunner(ctx, GroupRunner{
		Image:          m.image,
		Runtime:        m.runtime,
		AgentGroupID:   agentGroupID,
		GroupDir:       groupDir,
		ClaudeHome:     claudeHomeFor(groupDir),
		VaultDir:       m.vaultDir,
		CACertPath:     m.caCertPath,
		PluginsDir:     m.pluginsDir,
		ChannelSockDir: m.chanSockDir,
		ExtraMounts:    validated,
	})
}

// claudeHomeFor derives the persistent claude-home dir for a group from its
// sessions dir. groupDir is <data>/sessions/<id>; the home is
// <data>/claude-home/<id> - a sibling tree outside the sessions scan. This is the
// sole owner of the claude-home layout: it works in string ids derived from the
// path (the int64 group id is not available here), so keep the layout defined here
// rather than re-deriving it elsewhere.
func claudeHomeFor(groupDir string) string {
	id := filepath.Base(groupDir)                   // <id>
	dataDir := filepath.Dir(filepath.Dir(groupDir)) // <data>
	return filepath.Join(dataDir, "claude-home", id)
}

// validateExtra validates each requested mount against the allowlist, returning
// only the ones that pass. A nil allowlist (or a failed entry) yields no mount -
// fail closed (brief §6.3, §9).
func (m *Manager) validateExtra(reqs []mounts.Request) []mounts.Mount {
	if m.allowlist == nil || len(reqs) == 0 {
		return nil
	}
	var out []mounts.Mount
	for _, req := range reqs {
		mnt, err := m.allowlist.Validate(req)
		if err != nil {
			// Skip the entry; never silently widen access.
			continue
		}
		out = append(out, mnt)
	}
	return out
}

// Run launches a container per spec via `podman run -d` (detached) and returns its
// id from stdout; podman's stderr is captured and surfaced on failure. Teardown is
// Stop plus the sweep's idle-runner GC (internal/sweep). There is deliberately no
// podman `--restart` policy: a dead runner is recovered by the sweep relaunching
// it for any session with pending work, not by podman auto-restarting it.
func (m *Manager) Run(ctx context.Context, spec Spec) (string, error) {
	args := m.buildArgs(spec)
	cmd := execCommand(ctx, m.podmanBin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// Surface podman's stderr - a bare "exit status 125" is useless.
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return "", fmt.Errorf("runtime: podman run: %w: %s", err, msg)
		}
		return "", fmt.Errorf("runtime: podman run: %w", err)
	}
	return string(out), nil
}

// buildArgs assembles the `podman run` argument vector with the security
// defaults applied. Kept separate so it can be unit-tested without invoking
// podman.
func (m *Manager) buildArgs(spec Spec) []string {
	// NOTE: no --rm. A removed-on-exit container vanishes the instant its
	// process dies, which (a) hides crash logs/exit codes and (b) races the
	// sweep's recovery into a create→die→remove churn. We keep stopped
	// containers inspectable; the sweep's stateStopped path removes a stale one
	// before relaunching.
	args := []string{
		"run", "-d",
		"--user", "1000:1000", // non-root in container (brief §9)
		"--init", // PID-1 signal handling (brief §9)
	}
	if spec.Name != "" {
		args = append(args, "--name", spec.Name)
	}
	if spec.Runtime != "" && spec.Runtime != RuntimeCrun {
		args = append(args, "--runtime", string(spec.Runtime))
	}
	for _, mnt := range spec.Mounts {
		args = append(args, "-v", mnt.Arg())
	}
	for k, v := range spec.Env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}
	args = append(args, spec.Image)
	return args
}

// stopGraceSeconds is the SIGTERM-to-SIGKILL grace `podman stop` allows a runner to
// shut down cleanly. Passed explicitly rather than relying on podman's default so the
// grace is pinned regardless of the podman version.
const stopGraceSeconds = 10

// Stop stops a running container by id or name, giving it stopGraceSeconds to exit
// on SIGTERM before podman sends SIGKILL.
func (m *Manager) Stop(ctx context.Context, idOrName string) error {
	cmd := execCommand(ctx, m.podmanBin, "stop", "-t", fmt.Sprintf("%d", stopGraceSeconds), idOrName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("runtime: podman stop %q: %w", idOrName, err)
	}
	return nil
}
