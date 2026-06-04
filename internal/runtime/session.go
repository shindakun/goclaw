package runtime

import (
	"context"
	"fmt"
	"os"
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
	GroupDir     string         // host path to the dir holding this group's session subdirs
	ClaudeHome   string         // OPTIONAL host dir persisted as the container's ~/.claude
	VaultDir     string         // OPTIONAL host knowledge-vault dir, mounted rw at /vault
	CACertPath   string         // OPTIONAL host path to the credential-proxy CA, mounted RO
	PluginsDir   string         // OPTIONAL host plugins dir, mounted RO at /plugins
	ExtraMounts  []mounts.Mount // already allowlist-validated extra mounts (brief §6.3)
}

// claudeHomePath is where the container's claude CLI keeps its config + session
// history. Matches HOME=/home/agent in container/claude.Containerfile (a neutral
// home for uid 1000, not the base image's incidental "node" user).
const claudeHomePath = "/home/agent/.claude"

// vaultMountPath is where the knowledge vault is mounted in the container; the
// runner reads vaultMountPath/CLAUDE.md as its system prompt (brief §11).
const vaultMountPath = "/vault"

// pluginsMountPath is where the host's installed-plugins dir is mounted READ-ONLY.
// The in-container runner discovers and launches plugins from here; mounting it
// read-only keeps untrusted plugin code from modifying its own install.
const pluginsMountPath = "/plugins"

// PluginsContainerPath is the in-container path of the plugins mount, exported so
// the runner knows where to discover plugins.
func PluginsContainerPath() string { return pluginsMountPath }

// caCertMountPath is where the credential-proxy CA cert is mounted (read-only)
// in the container. The runner's trust env vars (NODE_EXTRA_CA_CERTS,
// GIT_SSL_CAINFO, SSL_CERT_FILE) point here so the agent's tools trust the
// proxy's intercepted TLS (brief §8).
const caCertMountPath = "/etc/goclaw/proxy-ca.pem"

// CACertContainerPath is where the credential-proxy CA cert lands in the
// container; the host points the agent's TLS-trust env vars at it.
func CACertContainerPath() string { return caCertMountPath }

// containerNamePrefix is the common prefix of every runner container's name.
const containerNamePrefix = "goclaw-"

// groupContainerName returns the runner container name for an agent group.
func groupContainerName(agentGroupID int64) string {
	return containerNamePrefix + fmt.Sprintf("%d", agentGroupID)
}

// containerName is a stable, podman-safe name derived from the agent group, so
// EnsureGroupRunner can check idempotently whether it's already running.
func (g GroupRunner) containerName() string {
	return groupContainerName(g.AgentGroupID)
}

// EnsureGroupRunner starts the runner container for an agent group if one isn't
// already running. It is safe to call on every inbound message and is robust to
// a name collision: a running container on the CURRENT image is left alone; a
// leftover stopped one - or a running one started from a DIFFERENT image (e.g.
// after switching GOCLAW_RUNNER_IMAGE) - is removed before launching. (Otherwise
// the config change would silently have no effect, and `podman run --name` on a
// stopped name fails with exit 125.) The group's sessions dir is mounted
// read-write at /sessions with :Z relabeling (brief §6.3).
func (m *Manager) EnsureGroupRunner(ctx context.Context, gr GroupRunner) error {
	// Serialize per group: the router and the sweep both call this concurrently,
	// and the inspect -> remove -> run sequence below must be atomic or two
	// callers can both launch a container for the same group (duplicate replies,
	// canceled in-flight work). See Manager.ensureMu.
	lock := m.groupLock(gr.AgentGroupID)
	lock.Lock()
	defer lock.Unlock()

	name := gr.containerName()

	info, err := m.inspectContainer(ctx, name)
	if err != nil {
		return err
	}
	switch info.state {
	case stateRunning:
		if m.runningCurrentImage(ctx, info, gr.Image) {
			return nil // already up on the current image
		}
		// Image changed under it (different tag, OR a rebuild of the same tag) -
		// replace so the new image takes effect.
		if err := m.remove(ctx, name); err != nil {
			return err
		}
	case stateStopped:
		// Stale container holding the name - remove it so the run can reuse it.
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

	groupMounts := []mounts.Mount{{
		HostPath:      hostDir,
		ContainerPath: sessionsMountPath,
		ReadWrite:     true, // runner reads inbound, writes outbound
	}}

	// Persist the container's ~/.claude so the CLI's conversation history (and
	// thus multi-turn --resume) survives the container being recreated. The host
	// dir must exist and be writable by the mapped uid; create it up front.
	if gr.ClaudeHome != "" {
		home, err := filepath.Abs(gr.ClaudeHome)
		if err != nil {
			return fmt.Errorf("runtime: resolve claude home %q: %w", gr.ClaudeHome, err)
		}
		if err := os.MkdirAll(home, 0o777); err != nil {
			return fmt.Errorf("runtime: create claude home %q: %w", home, err)
		}
		// Compose the agent's system prompt + skill symlinks into claude-home,
		// fresh each launch: the baked-in base (/app/CLAUDE.md), the coding skill
		// (always), and the librarian skill when a vault is mounted. The runner
		// reads the composed CLAUDE.md as its system prompt (see compose.go).
		if err := composeGroupPrompt(home, gr.VaultDir != ""); err != nil {
			return err
		}
		groupMounts = append(groupMounts, mounts.Mount{
			HostPath:      home,
			ContainerPath: claudeHomePath,
			ReadWrite:     true,
		})
	}

	// Mount the knowledge vault read-write at /vault so the agent can read its
	// CLAUDE.md (system prompt) and read/write notes (brief §11).
	if gr.VaultDir != "" {
		vault, err := filepath.Abs(gr.VaultDir)
		if err != nil {
			return fmt.Errorf("runtime: resolve vault dir %q: %w", gr.VaultDir, err)
		}
		groupMounts = append(groupMounts, mounts.Mount{
			HostPath:      vault,
			ContainerPath: vaultMountPath,
			ReadWrite:     true,
		})
	}

	// Mount the credential-proxy CA cert read-only so the agent's tools trust the
	// proxy's intercepted TLS (brief §8). The trust env vars point at this path.
	if gr.CACertPath != "" {
		ca, err := filepath.Abs(gr.CACertPath)
		if err != nil {
			return fmt.Errorf("runtime: resolve CA cert %q: %w", gr.CACertPath, err)
		}
		groupMounts = append(groupMounts, mounts.Mount{
			HostPath:      ca,
			ContainerPath: caCertMountPath,
			ReadWrite:     false,
		})
	}

	// Mount the installed-plugins dir READ-ONLY at /plugins. Plugins are untrusted
	// downloaded code; the in-container runner launches them inside this sandbox so
	// they never run on the host. Read-only so a plugin cannot rewrite its own
	// install or another plugin's.
	if gr.PluginsDir != "" {
		plugins, err := filepath.Abs(gr.PluginsDir)
		if err != nil {
			return fmt.Errorf("runtime: resolve plugins dir %q: %w", gr.PluginsDir, err)
		}
		// Only mount if it exists; an absent plugins dir is fine (no plugins).
		if _, statErr := os.Stat(plugins); statErr == nil {
			groupMounts = append(groupMounts, mounts.Mount{
				HostPath:      plugins,
				ContainerPath: pluginsMountPath,
				ReadWrite:     false,
			})
		}
	}

	spec := Spec{
		Name:    name,
		Image:   gr.Image,
		Runtime: gr.Runtime,
		Mounts:  append(groupMounts, gr.ExtraMounts...),
		Env:     m.env,
	}
	id, err := m.Run(ctx, spec)
	if err != nil {
		return fmt.Errorf("runtime: ensure group runner %q: %w", name, err)
	}
	id = strings.TrimSpace(id)

	// Verify the container actually came up. `podman run -d` reports success
	// once the container is created, even if its process dies immediately - so
	// confirm it's running, and if not, surface the exit code + logs instead of
	// silently leaving the group without a runner.
	if st, err := m.containerState(ctx, name); err == nil && st != stateRunning {
		detail := m.diagnose(ctx, name)
		return fmt.Errorf("runtime: runner %q exited immediately after launch (id %s): %s", name, id, detail)
	}
	return nil
}

// diagnose collects a one-line summary of why a container isn't running:
// its exit code and the tail of its logs. Best-effort - used only for error
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

// containerInfo is the state of a named container plus the image it runs.
type containerInfo struct {
	state   containerStatus
	image   string // image reference, e.g. localhost/goclaw-claude:latest
	imageID string // image ID (digest prefix) - distinguishes same-tag rebuilds
}

// containerState reports whether a container with the given name exists and
// whether it is running.
func (m *Manager) containerState(ctx context.Context, name string) (containerStatus, error) {
	info, err := m.inspectContainer(ctx, name)
	return info.state, err
}

// inspectContainer reports a named container's state and, when it exists, the
// image and image ID it was started from. It lists all containers (running and
// not) by exact name, reading the State column so a name reserved by a
// non-running container is detected - that reservation is what causes
// `podman run --name` to fail. The image ID lets EnsureGroupRunner replace a
// container whose image changed, INCLUDING a rebuild under the same tag (which a
// name comparison alone misses). Fields are tab-delimited.
func (m *Manager) inspectContainer(ctx context.Context, name string) (containerInfo, error) {
	cmd := execCommand(ctx, m.podmanBin,
		"ps", "--all", "--filter", "name=^"+name+"$", "--format", "{{.Names}}\t{{.State}}\t{{.Image}}\t{{.ImageID}}")
	out, err := cmd.Output()
	if err != nil {
		return containerInfo{}, fmt.Errorf("runtime: podman ps: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 4 || fields[0] != name {
			continue
		}
		info := containerInfo{
			image:   strings.TrimSpace(fields[2]),
			imageID: strings.TrimSpace(fields[3]),
		}
		if fields[1] == "running" {
			info.state = stateRunning
		} else {
			info.state = stateStopped
		}
		return info, nil
	}
	return containerInfo{state: stateAbsent}, nil
}

// currentImageID returns the image ID for a reference (the rebuilt-under-same-tag
// discriminator). Empty if the image isn't present locally (e.g. pulled on run),
// in which case the caller falls back to a name comparison.
func (m *Manager) currentImageID(ctx context.Context, ref string) string {
	out, err := execCommand(ctx, m.podmanBin,
		"image", "inspect", ref, "--format", "{{.ID}}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// runningCurrentImage reports whether a running container is on the desired
// image. It prefers an image-ID match (so a rebuild under the same tag is
// detected as a change), and falls back to comparing image names when the ID
// can't be resolved locally.
func (m *Manager) runningCurrentImage(ctx context.Context, info containerInfo, desired string) bool {
	if want := m.currentImageID(ctx, desired); want != "" && info.imageID != "" {
		return sameImageID(info.imageID, want)
	}
	return sameImage(info.image, desired)
}

// sameImageID compares two podman image IDs, tolerating the short/long forms
// (podman ps reports a 12-char prefix; image inspect a full or sha256: id).
func sameImageID(a, b string) bool {
	a = strings.TrimPrefix(a, "sha256:")
	b = strings.TrimPrefix(b, "sha256:")
	if a == "" || b == "" {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return strings.HasPrefix(b, a)
}

// sameImage compares a container's reported image against a desired image,
// tolerating registry/namespace prefixes podman adds (e.g. a desired
// "goclaw-claude:latest" matches a reported "localhost/goclaw-claude:latest").
// An untagged desired ref is treated as ":latest".
func sameImage(reported, desired string) bool {
	r, d := normalizeImage(reported), normalizeImage(desired)
	if r == d {
		return true
	}
	// Match on the trailing path component (handles localhost/ and registry/ns
	// prefixes on either side).
	return lastPathComponent(r) == lastPathComponent(d)
}

func normalizeImage(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	// Add a default :latest tag if none present (ignore any digest form).
	name := ref
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		name = ref[i+1:]
	}
	if !strings.Contains(name, ":") && !strings.Contains(name, "@") {
		ref += ":latest"
	}
	return ref
}

func lastPathComponent(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
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

// RunningGroupIDs returns the agent group ids that currently have a running
// runner container. Used by the sweep's GC to find idle runners to reap.
func (m *Manager) RunningGroupIDs(ctx context.Context) ([]int64, error) {
	cmd := execCommand(ctx, m.podmanBin,
		"ps", "--filter", "name=^"+containerNamePrefix, "--format", "{{.Names}}")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("runtime: podman ps: %w", err)
	}
	var ids []int64
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name := strings.TrimSpace(line)
		if !strings.HasPrefix(name, containerNamePrefix) {
			continue
		}
		var id int64
		// The runner name is exactly goclaw-<id>; parse the suffix.
		if _, err := fmt.Sscanf(name, containerNamePrefix+"%d", &id); err != nil {
			continue // not a group runner (or an unexpected name) - skip
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// StopGroupRunner stops and removes the runner container for an agent group.
// Removing (not just stopping) frees the name so a later EnsureGroupRunner can
// relaunch cleanly. A non-existent container is not an error.
func (m *Manager) StopGroupRunner(ctx context.Context, agentGroupID int64) error {
	name := groupContainerName(agentGroupID)
	state, err := m.containerState(ctx, name)
	if err != nil {
		return err
	}
	if state == stateAbsent {
		return nil
	}
	return m.remove(ctx, name)
}
