package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestComposeGroupPrompt_NoVault(t *testing.T) {
	home := t.TempDir()
	if err := composeGroupPrompt(home, false); err != nil {
		t.Fatalf("compose: %v", err)
	}

	// Entry point imports the shared base.
	body, err := os.ReadFile(filepath.Join(home, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("read composed CLAUDE.md: %v", err)
	}
	if want := "@./.claude-shared.md"; !contains(string(body), want) {
		t.Fatalf("composed CLAUDE.md missing %q; got:\n%s", want, body)
	}

	// .claude-shared.md -> /app/CLAUDE.md (container path, dangles on host).
	if tgt, err := os.Readlink(filepath.Join(home, ".claude-shared.md")); err != nil || tgt != baseClaudeMdContainerPath {
		t.Fatalf("shared link target = %q, err %v; want %q", tgt, err, baseClaudeMdContainerPath)
	}

	// coding skill present; librarian absent without a vault.
	if tgt, err := os.Readlink(filepath.Join(home, "skills", "coding")); err != nil || tgt != skillsContainerBase+"/coding" {
		t.Fatalf("coding link = %q, err %v", tgt, err)
	}
	if _, err := os.Lstat(filepath.Join(home, "skills", "librarian")); !os.IsNotExist(err) {
		t.Fatalf("librarian skill should be absent without a vault; lstat err = %v", err)
	}
}

func TestComposeGroupPrompt_WithVault(t *testing.T) {
	home := t.TempDir()
	if err := composeGroupPrompt(home, true); err != nil {
		t.Fatalf("compose: %v", err)
	}
	if tgt, err := os.Readlink(filepath.Join(home, "skills", "librarian")); err != nil || tgt != vaultLibrarianSkillPath {
		t.Fatalf("librarian link = %q, err %v; want %q", tgt, err, vaultLibrarianSkillPath)
	}
	if _, err := os.Readlink(filepath.Join(home, "skills", "coding")); err != nil {
		t.Fatalf("coding link should still be present with a vault: %v", err)
	}
}

// TestComposeGroupPrompt_PrunesLibrarianWhenVaultRemoved is the important one:
// composing WITH a vault then again WITHOUT must remove the librarian symlink,
// so unmounting a vault doesn't leave a dangling librarian skill behind.
func TestComposeGroupPrompt_PrunesLibrarianWhenVaultRemoved(t *testing.T) {
	home := t.TempDir()
	if err := composeGroupPrompt(home, true); err != nil {
		t.Fatalf("compose with vault: %v", err)
	}
	if _, err := os.Readlink(filepath.Join(home, "skills", "librarian")); err != nil {
		t.Fatalf("expected librarian after vault compose: %v", err)
	}
	if err := composeGroupPrompt(home, false); err != nil {
		t.Fatalf("compose without vault: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, "skills", "librarian")); !os.IsNotExist(err) {
		t.Fatalf("librarian should be pruned after recompose without vault; lstat err = %v", err)
	}
	// coding survives the recompose.
	if _, err := os.Readlink(filepath.Join(home, "skills", "coding")); err != nil {
		t.Fatalf("coding link lost on recompose: %v", err)
	}
}

// TestSyncSkillSymlinks_LeavesRealFilesAlone: a non-symlink in the skills dir
// (e.g. a real directory the user created) must not be deleted by pruning.
func TestSyncSkillSymlinks_LeavesRealFilesAlone(t *testing.T) {
	skillsDir := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(filepath.Join(skillsDir, "real-thing"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := syncSkillSymlinks(skillsDir, map[string]string{"coding": "/app/skills/coding"}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "real-thing")); err != nil {
		t.Fatalf("real directory was removed by prune: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
