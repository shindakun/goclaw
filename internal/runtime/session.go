package runtime

import (
	"context"
	"fmt"
	"path/filepath"
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
// already running. It is safe to call on every inbound message and is robust to
// a name collision: a running container is left alone; a leftover stopped one
// with the same name is removed before launching (otherwise `podman run --name`
// fails with exit 125). The session dir is mounted read-write at /session with
// :Z relabeling (brief §6.3).
func (m *Manager) EnsureSessionRunner(ctx context.Context, sr SessionRunner) error {
	name := sr.containerName()

	state, err := m.containerState(ctx, name)
	if err != nil {
		return err
	}
	switch state {
	case stateRunning:
		return nil // already up
	case stateStopped:
		// Stale container holding the name — remove it so the run can reuse it.
		if err := m.remove(ctx, name); err != nil {
			return err
		}
	}

	// The mount source MUST be absolute: podman treats a relative `-v` source
	// as a named volume (and a path with '/' is an invalid volume name), which
	// is what produced the "creating named volume" error. Resolve it here.
	hostDir, err := filepath.Abs(sr.SessionDir)
	if err != nil {
		return fmt.Errorf("runtime: resolve session dir %q: %w", sr.SessionDir, err)
	}

	spec := Spec{
		Name:    name,
		Image:   sr.Image,
		Runtime: sr.Runtime,
		Mounts: []mounts.Mount{{
			HostPath:      hostDir,
			ContainerPath: sessionMountPath,
			ReadWrite:     true, // runner reads inbound, writes outbound
		}},
	}
	if _, err := m.Run(ctx, spec); err != nil {
		return fmt.Errorf("runtime: ensure session runner %q: %w", name, err)
	}
	return nil
}

// containerStatus is the coarse state of a named container.
type containerStatus int

const (
	stateAbsent  containerStatus = iota // no container with this name
	stateRunning                        // exists and running
	stateStopped                        // exists but not running (created/exited/...)
)

// containerState reports whether a container with the given name exists and
// whether it is running. It lists all containers (running and not) by exact
// name, reading the State column so a name reserved by a non-running container
// is detected — that reservation is what causes `podman run --name` to fail.
func (m *Manager) containerState(ctx context.Context, name string) (containerStatus, error) {
	cmd := execCommand(ctx, m.podmanBin,
		"ps", "--all", "--filter", "name=^"+name+"$", "--format", "{{.Names}} {{.State}}")
	out, err := cmd.Output()
	if err != nil {
		return stateAbsent, fmt.Errorf("runtime: podman ps: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != name {
			continue
		}
		if fields[1] == "running" {
			return stateRunning, nil
		}
		return stateStopped, nil
	}
	return stateAbsent, nil
}

// IsRunning reports whether a container with the given name is currently
// running.
func (m *Manager) IsRunning(ctx context.Context, name string) (bool, error) {
	state, err := m.containerState(ctx, name)
	if err != nil {
		return false, err
	}
	return state == stateRunning, nil
}

// remove force-removes a container by name (used to clear a stale name).
func (m *Manager) remove(ctx context.Context, name string) error {
	cmd := execCommand(ctx, m.podmanBin, "rm", "-f", name)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("runtime: podman rm %q: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
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
