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
	podmanBin string            // path/name of the podman binary
	image     string            // runner image to launch
	runtime   Runtime           // OCI runtime for launched containers
	allowlist *mounts.Allowlist // validates extra group mounts (may be nil)
	env       map[string]string // env vars set on every launched container
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
	return &Manager{podmanBin: binary, image: image, runtime: rt, allowlist: allowlist}
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

// EnsureRunner implements the RunnerEnsurer interface used by the router and
// sweep: it ensures a runner container is up for the given agent group,
// launching one (mounting groupDir at /sessions, plus any extra mounts that
// pass allowlist validation) if not. Idempotent - a running container is left
// alone. extra mounts that fail validation are skipped (fail closed) and logged
// by the caller via the returned-from-Validate errors collected here.
func (m *Manager) EnsureRunner(ctx context.Context, agentGroupID int64, groupDir string, extra ...mounts.Request) error {
	validated := m.validateExtra(extra)
	return m.EnsureGroupRunner(ctx, GroupRunner{
		Image:        m.image,
		Runtime:      m.runtime,
		AgentGroupID: agentGroupID,
		GroupDir:     groupDir,
		ClaudeHome:   claudeHomeFor(groupDir),
		ExtraMounts:  validated,
	})
}

// claudeHomeFor derives the persistent claude-home dir for a group from its
// sessions dir. groupDir is <data>/sessions/<id>; the home is
// <data>/claude-home/<id> - a sibling tree outside the sessions scan.
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

// Run launches a container per spec via `podman run`. Returns the container id.
//
// TODO: capture stdout/stderr, detached vs. foreground, restart policy, and
// teardown. For v0 this only assembles the argv and runs it.
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

// Stop stops a running container by id or name.
//
// TODO: implement `podman stop` with a grace period.
func (m *Manager) Stop(ctx context.Context, idOrName string) error {
	cmd := execCommand(ctx, m.podmanBin, "stop", idOrName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("runtime: podman stop %q: %w", idOrName, err)
	}
	return nil
}
