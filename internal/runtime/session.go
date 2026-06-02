package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/shindakun/goclaw/internal/mounts"
)

// The container mount point for an agent group's sessions tree. The runner
// image's entrypoint points at this path; the runner serves every session
// subdirectory beneath it (see container/runner.Containerfile).
const sessionsMountPath = "/sessions"

// GroupRunner describes a per-agent-group runner container to ensure is running.
// One container per agent group serves all that group's sessions (brief §3.3):
// it mounts the group's sessions directory and the runner loops over the session
// subdirectories within.
type GroupRunner struct {
	Image        string  // runner image, e.g. "goclaw-runner:latest"
	Runtime      Runtime // OCI runtime (crun default)
	AgentGroupID int64
	GroupDir     string // host path to the dir holding this group's session subdirs
}

// containerName is a stable, podman-safe name derived from the agent group, so
// EnsureGroupRunner can check idempotently whether it's already running.
func (g GroupRunner) containerName() string {
	return "goclaw-" + fmt.Sprintf("%d", g.AgentGroupID)
}

// EnsureGroupRunner starts the runner container for an agent group if one isn't
// already running. It is safe to call on every inbound message and is robust to
// a name collision: a running container is left alone; a leftover stopped one
// with the same name is removed before launching (otherwise `podman run --name`
// fails with exit 125). The group's sessions dir is mounted read-write at
// /sessions with :Z relabeling (brief §6.3).
func (m *Manager) EnsureGroupRunner(ctx context.Context, gr GroupRunner) error {
	name := gr.containerName()

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
	hostDir, err := filepath.Abs(gr.GroupDir)
	if err != nil {
		return fmt.Errorf("runtime: resolve group dir %q: %w", gr.GroupDir, err)
	}

	spec := Spec{
		Name:    name,
		Image:   gr.Image,
		Runtime: gr.Runtime,
		Mounts: []mounts.Mount{{
			HostPath:      hostDir,
			ContainerPath: sessionsMountPath,
			ReadWrite:     true, // runner reads inbound, writes outbound
		}},
	}
	id, err := m.Run(ctx, spec)
	if err != nil {
		return fmt.Errorf("runtime: ensure group runner %q: %w", name, err)
	}
	id = strings.TrimSpace(id)

	// Verify the container actually came up. `podman run -d` reports success
	// once the container is created, even if its process dies immediately — so
	// confirm it's running, and if not, surface the exit code + logs instead of
	// silently leaving the group without a runner.
	if st, err := m.containerState(ctx, name); err == nil && st != stateRunning {
		detail := m.diagnose(ctx, name)
		return fmt.Errorf("runtime: runner %q exited immediately after launch (id %s): %s", name, id, detail)
	}
	return nil
}

// diagnose collects a one-line summary of why a container isn't running:
// its exit code and the tail of its logs. Best-effort — used only for error
// messages, so failures to gather detail are ignored.
func (m *Manager) diagnose(ctx context.Context, name string) string {
	exit := "?"
	if out, err := execCommand(ctx, m.podmanBin,
		"inspect", name, "--format", "{{.State.ExitCode}} oom={{.State.OOMKilled}} err={{.State.Error}}").Output(); err == nil {
		exit = strings.TrimSpace(string(out))
	}
	logs := ""
	if out, err := execCommand(ctx, m.podmanBin, "logs", "--tail", "5", name).CombinedOutput(); err == nil {
		logs = strings.TrimSpace(string(out))
	}
	return fmt.Sprintf("exit=%s logs=%q", exit, logs)
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
