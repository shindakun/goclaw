package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/shindakun/goclaw/internal/mounts"
)

// The container mount point for a session's DB pair. The runner image's
// entrypoint points at this path (see container/runner.Containerfile).
const sessionMountPath = "/session"

// SessionRunner describes a per-session runner container to ensure is running.
// v0 runs one container per session (the stub runner operates on a single
// session dir); the real per-agent-group runner will broaden this later
// (brief §3.3, §4).
type SessionRunner struct {
	Image        string  // runner image, e.g. "goclaw-runner:latest"
	Runtime      Runtime // OCI runtime (crun default)
	AgentGroupID int64
	SessionKey   string // sanitized form is used for the container name
	SessionDir   string // host path to the dir holding inbound.db + outbound.db
}

// containerName is a stable, podman-safe name derived from the session, so
// EnsureSessionRunner can check idempotently whether it's already running.
func (s SessionRunner) containerName() string {
	return "goclaw-" + fmt.Sprintf("%d", s.AgentGroupID) + "-" + safeName(s.SessionKey)
}

// EnsureSessionRunner starts the runner container for a session if one isn't
// already running. It is safe to call on every inbound message: a running
// container is left alone. The session dir is mounted read-write at /session
// with :Z relabeling (brief §6.3).
func (m *Manager) EnsureSessionRunner(ctx context.Context, sr SessionRunner) error {
	name := sr.containerName()
	running, err := m.IsRunning(ctx, name)
	if err != nil {
		return err
	}
	if running {
		return nil // already up
	}

	spec := Spec{
		Name:    name,
		Image:   sr.Image,
		Runtime: sr.Runtime,
		Mounts: []mounts.Mount{{
			HostPath:      sr.SessionDir,
			ContainerPath: sessionMountPath,
			ReadWrite:     true, // runner reads inbound, writes outbound
		}},
	}
	if _, err := m.Run(ctx, spec); err != nil {
		return fmt.Errorf("runtime: ensure session runner %q: %w", name, err)
	}
	return nil
}

// IsRunning reports whether a container with the given name is currently
// running, via `podman ps`.
func (m *Manager) IsRunning(ctx context.Context, name string) (bool, error) {
	cmd := execCommand(ctx, m.podmanBin,
		"ps", "--filter", "name=^"+name+"$", "--format", "{{.Names}}")
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("runtime: podman ps: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}
	return false, nil
}

// safeName reduces a session key to a podman-safe container-name segment
// ([a-zA-Z0-9_.-]); anything else becomes '-'.
func safeName(key string) string {
	b := make([]rune, 0, len(key))
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b = append(b, r)
		default:
			b = append(b, '-')
		}
	}
	if len(b) == 0 {
		return "x"
	}
	return string(b)
}
