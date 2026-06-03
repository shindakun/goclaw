// Package mounts loads the external mount allowlist and validates requested
// bind mounts before they reach `podman run`. This is the single most
// security-critical package in the host: a bug here becomes a
// host-filesystem-escape (brief §6.3, §9, §10).
//
// Guarantees enforced here:
//   - Fail closed: if the allowlist file is absent, NO extra mounts are allowed.
//   - Symlink resolution BEFORE validation, so a symlinked host path can't
//     smuggle access outside the allowlist.
//   - Reject ".." and non-absolute or colon-containing container paths (the
//     colon guard blocks `-v` option injection against podman, brief §6.3).
//   - SELinux relabeling: rw mounts get ":Z" (private), ro mounts ":ro,Z".
package mounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Validation errors. Callers may match these with errors.Is.
var (
	ErrNoAllowlist     = errors.New("mounts: allowlist absent - failing closed, no extra mounts")
	ErrNotAllowed      = errors.New("mounts: host path not under any allowlist entry")
	ErrContainerPath   = errors.New("mounts: invalid container path")
	ErrReadWriteDenied = errors.New("mounts: read-write requested but entry is read-only")
	ErrTraversal       = errors.New("mounts: path traversal ('..') rejected")
)

// AllowEntry is one rule in the external allowlist
// (~/.config/goclaw/mount-allowlist.json - outside the project root, never
// itself mounted, brief §6.3).
type AllowEntry struct {
	HostPath  string `json:"host_path"`  // a host directory prefix
	ReadWrite bool   `json:"read_write"` // may this prefix be mounted rw?
}

// Allowlist is the loaded set of entries.
type Allowlist struct {
	entries []AllowEntry
}

// Request is a requested bind mount from an agent-group config.
type Request struct {
	HostPath      string // host source (may be a symlink; resolved before checks)
	ContainerPath string // absolute path inside the container
	ReadWrite     bool
}

// Mount is a validated mount, ready to render as a `-v` argument.
type Mount struct {
	HostPath      string // resolved, absolute
	ContainerPath string
	ReadWrite     bool
}

// LoadAllowlist reads the allowlist JSON. A missing file is NOT an error from
// loading's perspective - it yields an empty allowlist that denies everything
// (fail closed). Callers that requested mounts will then get ErrNotAllowed.
func LoadAllowlist(path string) (*Allowlist, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Allowlist{}, nil // empty → denies all extra mounts
	}
	if err != nil {
		return nil, fmt.Errorf("mounts: read allowlist %q: %w", path, err)
	}
	var entries []AllowEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("mounts: parse allowlist: %w", err)
	}
	// Normalize host prefixes once, up front: clean, and resolve symlinks so an
	// entry like "/var/foo" matches a request that resolves to "/private/var/foo"
	// (e.g. on macOS). An entry that doesn't yet exist on disk is kept as-cleaned
	// rather than dropped.
	for i := range entries {
		entries[i].HostPath = resolveOrClean(entries[i].HostPath)
	}
	return &Allowlist{entries: entries}, nil
}

// Validate resolves and checks a single requested mount against the allowlist.
func (a *Allowlist) Validate(req Request) (Mount, error) {
	// 1. Container path must be absolute, free of "..", and contain no colon
	//    (the colon guard prevents `-v host:container:opts` injection).
	if err := validateContainerPath(req.ContainerPath); err != nil {
		return Mount{}, err
	}

	// 2. Resolve symlinks on the HOST path BEFORE comparing to the allowlist,
	//    so a symlink can't point outside an allowed prefix.
	resolved, err := filepath.EvalSymlinks(req.HostPath)
	if err != nil {
		return Mount{}, fmt.Errorf("mounts: resolve host path %q: %w", req.HostPath, err)
	}
	resolved = filepath.Clean(resolved)
	if strings.Contains(resolved, "..") {
		return Mount{}, ErrTraversal
	}

	// 3. The resolved path must sit under an allowlist entry, and rw must be
	//    permitted by that entry.
	if len(a.entries) == 0 {
		return Mount{}, ErrNoAllowlist
	}
	entry, ok := a.match(resolved)
	if !ok {
		return Mount{}, fmt.Errorf("%w: %s", ErrNotAllowed, resolved)
	}
	if req.ReadWrite && !entry.ReadWrite {
		return Mount{}, fmt.Errorf("%w: %s", ErrReadWriteDenied, resolved)
	}

	return Mount{
		HostPath:      resolved,
		ContainerPath: req.ContainerPath,
		ReadWrite:     req.ReadWrite,
	}, nil
}

// resolveOrClean returns the symlink-resolved, cleaned form of p, falling back
// to a plain Clean if the path can't be resolved (e.g. doesn't exist yet).
func resolveOrClean(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(p)
}

// match returns the allowlist entry whose HostPath is a path-prefix of p.
func (a *Allowlist) match(p string) (AllowEntry, bool) {
	for _, e := range a.entries {
		if p == e.HostPath || strings.HasPrefix(p, e.HostPath+string(os.PathSeparator)) {
			return e, true
		}
	}
	return AllowEntry{}, false
}

// validateContainerPath enforces the container-side rules.
func validateContainerPath(p string) error {
	if p == "" || !strings.HasPrefix(p, "/") {
		return fmt.Errorf("%w: must be absolute: %q", ErrContainerPath, p)
	}
	if strings.Contains(p, "..") {
		return fmt.Errorf("%w: %w", ErrContainerPath, ErrTraversal)
	}
	if strings.Contains(p, ":") {
		// A colon would let an attacker append `:rw,...` and inject `-v` options.
		return fmt.Errorf("%w: colon not allowed: %q", ErrContainerPath, p)
	}
	return nil
}

// Arg renders a validated mount as a `podman run -v` value, with SELinux
// relabeling: ":Z" for a private per-container label (brief §6.3). Read-only
// mounts add the "ro" option.
func (m Mount) Arg() string {
	opts := "Z" // private SELinux relabel
	if !m.ReadWrite {
		opts = "ro,Z"
	}
	return fmt.Sprintf("%s:%s:%s", m.HostPath, m.ContainerPath, opts)
}
