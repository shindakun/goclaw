// Package runtime manages per-agent-group container lifecycle on Podman
// (brief §6). v0 shells out to the `podman` CLI — simplest, easy to audit, and
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
	podmanBin string  // path/name of the podman binary
	image     string  // runner image to launch
	runtime   Runtime // OCI runtime for launched containers
}

// New constructs a Manager. binary defaults to "podman" and runtime to crun if
// empty. image is the runner image launched by EnsureRunner.
func New(binary, image string, rt Runtime) *Manager {
	if binary == "" {
		binary = "podman"
	}
	if rt == "" {
		rt = RuntimeCrun
	}
	return &Manager{podmanBin: binary, image: image, runtime: rt}
}

// EnsureRunner implements router.RunnerEnsurer: it ensures a runner container is
// up for the given session, launching one (mounting sessionDir at /session) if
// not. Idempotent — a running container is left alone.
func (m *Manager) EnsureRunner(ctx context.Context, agentGroupID int64, sessionKey, sessionDir string) error {
	return m.EnsureSessionRunner(ctx, SessionRunner{
		Image:        m.image,
		Runtime:      m.runtime,
		AgentGroupID: agentGroupID,
		SessionKey:   sessionKey,
		SessionDir:   sessionDir,
	})
}

// Run launches a container per spec via `podman run`. Returns the container id.
//
// TODO: capture stdout/stderr, detached vs. foreground, restart policy, and
// teardown. For v0 this only assembles the argv and runs it.
func (m *Manager) Run(ctx context.Context, spec Spec) (string, error) {
	args := m.buildArgs(spec)
	cmd := execCommand(ctx, m.podmanBin, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("runtime: podman run: %w", err)
	}
	return string(out), nil
}

// buildArgs assembles the `podman run` argument vector with the security
// defaults applied. Kept separate so it can be unit-tested without invoking
// podman.
func (m *Manager) buildArgs(spec Spec) []string {
	args := []string{
		"run", "--rm", "-d",
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
