package mounts

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// newAllowlist builds an in-memory allowlist for tests without touching disk.
// It resolves symlinks on entry paths the same way LoadAllowlist does, so
// entries match request paths that the validator resolves (e.g. macOS
// /var → /private/var).
func newAllowlist(entries ...AllowEntry) *Allowlist {
	for i := range entries {
		entries[i].HostPath = resolveOrClean(entries[i].HostPath)
	}
	return &Allowlist{entries: entries}
}

func TestValidateContainerPath(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"absolute ok", "/vault", false},
		{"relative rejected", "vault", true},
		{"empty rejected", "", true},
		{"traversal rejected", "/vault/../etc", true},
		{"colon rejected (option injection)", "/vault:rw", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateContainerPath(tc.path)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q, got nil", tc.path)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.path, err)
			}
		})
	}
}

func TestValidate_FailClosedWhenAllowlistEmpty(t *testing.T) {
	dir := t.TempDir()
	a := newAllowlist() // no entries
	_, err := a.Validate(Request{HostPath: dir, ContainerPath: "/vault"})
	if !errors.Is(err, ErrNoAllowlist) {
		t.Fatalf("expected ErrNoAllowlist, got %v", err)
	}
}

func TestValidate_NotUnderAllowlist(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	other := filepath.Join(base, "other")
	mustMkdir(t, allowed)
	mustMkdir(t, other)

	a := newAllowlist(AllowEntry{HostPath: allowed, ReadWrite: false})
	_, err := a.Validate(Request{HostPath: other, ContainerPath: "/vault"})
	if !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("expected ErrNotAllowed, got %v", err)
	}
}

func TestValidate_ReadWriteDeniedOnReadOnlyEntry(t *testing.T) {
	dir := t.TempDir()
	a := newAllowlist(AllowEntry{HostPath: dir, ReadWrite: false})
	_, err := a.Validate(Request{HostPath: dir, ContainerPath: "/vault", ReadWrite: true})
	if !errors.Is(err, ErrReadWriteDenied) {
		t.Fatalf("expected ErrReadWriteDenied, got %v", err)
	}
}

func TestValidate_AllowedReadOnly(t *testing.T) {
	dir := t.TempDir()
	resolved := resolveOrClean(dir) // Validate returns the symlink-resolved path
	a := newAllowlist(AllowEntry{HostPath: dir, ReadWrite: true})
	m, err := a.Validate(Request{HostPath: dir, ContainerPath: "/vault", ReadWrite: false})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := m.Arg(), resolved+":/vault:ro,Z"; got != want {
		t.Fatalf("Arg() = %q, want %q", got, want)
	}
}

func TestValidate_AllowedReadWriteHasZRelabel(t *testing.T) {
	dir := t.TempDir()
	resolved := resolveOrClean(dir)
	a := newAllowlist(AllowEntry{HostPath: dir, ReadWrite: true})
	m, err := a.Validate(Request{HostPath: dir, ContainerPath: "/vault", ReadWrite: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := m.Arg(), resolved+":/vault:Z"; got != want {
		t.Fatalf("Arg() = %q, want %q", got, want)
	}
}

// A symlink pointing OUTSIDE the allowlist must be rejected after resolution.
func TestValidate_SymlinkEscapeRejected(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	secret := filepath.Join(base, "secret")
	mustMkdir(t, allowed)
	mustMkdir(t, secret)

	// link lives under the allowed dir but points at the secret dir.
	link := filepath.Join(allowed, "link")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	a := newAllowlist(AllowEntry{HostPath: allowed, ReadWrite: true})
	_, err := a.Validate(Request{HostPath: link, ContainerPath: "/vault"})
	if !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("expected ErrNotAllowed after symlink resolution, got %v", err)
	}
}

func TestLoadAllowlist_MissingFileFailsClosed(t *testing.T) {
	a, err := LoadAllowlist(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("missing file should not error from loading: %v", err)
	}
	_, vErr := a.Validate(Request{HostPath: t.TempDir(), ContainerPath: "/vault"})
	if !errors.Is(vErr, ErrNoAllowlist) {
		t.Fatalf("expected ErrNoAllowlist from empty allowlist, got %v", vErr)
	}
}

// sensitiveLeaves enumerates the kinds of credential/secret locations that must
// never be reachable as a bind mount. It is the goclaw analog of a "forbidden
// zones" list, used here as ADVERSARIAL TEST DATA rather than as a runtime
// denylist: goclaw's model is an allowlist (deny by default), which is stronger
// than a denylist (allow by default, leak on omission). The point of this test is
// to prove the allowlist model holds for each of these even when an operator
// scopes the allowlist to a parent directory that happens to sit beside them.
var sensitiveLeaves = []string{
	".ssh",
	".aws",
	".gnupg",
	".kube",
	".docker",
	".npmrc",
	".netrc",
	".git-credentials",
	".config/gcloud",
	".config/gh",
	".password-store",
	".env",
}

// A request to mount a sensitive path that is NOT under the allowlist is denied.
// Models an operator with a narrowly-scoped allowlist (only a project dir): the
// agent must not be able to request ~/.ssh and have it pass.
func TestValidate_SensitivePathsNotUnderAllowlist(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "project")
	mustMkdir(t, project)
	a := newAllowlist(AllowEntry{HostPath: project, ReadWrite: true})

	for _, leaf := range sensitiveLeaves {
		t.Run(leaf, func(t *testing.T) {
			p := filepath.Join(home, leaf)
			mustMkdir(t, p)
			_, err := a.Validate(Request{HostPath: p, ContainerPath: "/mnt"})
			if !errors.Is(err, ErrNotAllowed) {
				t.Fatalf("mounting %s should be denied (not under allowlist), got %v", leaf, err)
			}
		})
	}
}

// A symlink that lives INSIDE the allowlisted dir but points at a sensitive path
// outside it must be rejected after symlink resolution. This is the smuggling
// case: the requested path looks allowed, but resolves elsewhere.
func TestValidate_SymlinkToSensitivePathRejected(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "project")
	mustMkdir(t, project)
	a := newAllowlist(AllowEntry{HostPath: project, ReadWrite: true})

	for _, leaf := range sensitiveLeaves {
		t.Run(leaf, func(t *testing.T) {
			target := filepath.Join(home, leaf)
			mustMkdir(t, target)
			// link sits under the allowed project dir but points at the secret.
			link := filepath.Join(project, "link-"+filepath.Base(leaf))
			if err := os.Symlink(target, link); err != nil {
				t.Fatalf("symlink: %v", err)
			}
			_, err := a.Validate(Request{HostPath: link, ContainerPath: "/mnt"})
			if !errors.Is(err, ErrNotAllowed) {
				t.Fatalf("symlink smuggling %s should be denied after resolution, got %v", leaf, err)
			}
		})
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}
