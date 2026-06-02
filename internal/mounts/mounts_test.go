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

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}
