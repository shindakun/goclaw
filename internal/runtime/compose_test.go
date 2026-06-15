package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestComposeGroupPrompt_NoVault(t *testing.T) {
	home := t.TempDir()
	if err := composeGroupPrompt(home, false, false); err != nil {
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

	// Without a vault there is no CRITICAL_FACTS import to resolve.
	if bad := "@" + vaultCriticalFactsPath; contains(string(body), bad) {
		t.Fatalf("composed CLAUDE.md should not import %q without a vault; got:\n%s", bad, body)
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
	if err := composeGroupPrompt(home, true, false); err != nil {
		t.Fatalf("compose: %v", err)
	}

	// With a vault, the entry point imports the always-load L0 facts file so it
	// is present on every turn regardless of the librarian skill.
	body, err := os.ReadFile(filepath.Join(home, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("read composed CLAUDE.md: %v", err)
	}
	if want := "@" + vaultCriticalFactsPath; !contains(string(body), want) {
		t.Fatalf("composed CLAUDE.md missing %q with a vault; got:\n%s", want, body)
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
	if err := composeGroupPrompt(home, true, false); err != nil {
		t.Fatalf("compose with vault: %v", err)
	}
	if _, err := os.Readlink(filepath.Join(home, "skills", "librarian")); err != nil {
		t.Fatalf("expected librarian after vault compose: %v", err)
	}
	if err := composeGroupPrompt(home, false, false); err != nil {
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

func TestComposeGroupPrompt_IntrospectionGatedOnEvents(t *testing.T) {
	home := t.TempDir()

	// Events off: introspection absent, coding present.
	if err := composeGroupPrompt(home, false, false); err != nil {
		t.Fatalf("compose: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, "skills", "introspection")); !os.IsNotExist(err) {
		t.Fatalf("introspection should be absent without the event log; lstat err = %v", err)
	}

	// Events on: introspection linked to the baked-in /app/skills/introspection.
	if err := composeGroupPrompt(home, false, true); err != nil {
		t.Fatalf("compose with events: %v", err)
	}
	if tgt, err := os.Readlink(filepath.Join(home, "skills", "introspection")); err != nil || tgt != introspectionSkillPath {
		t.Fatalf("introspection link = %q, err %v; want %q", tgt, err, introspectionSkillPath)
	}
	if _, err := os.Readlink(filepath.Join(home, "skills", "coding")); err != nil {
		t.Fatalf("coding link should still be present with events: %v", err)
	}
}

// Dropping the events mount (e.g. host goes multi-group, un-gating it) must prune
// the introspection link, so the agent does not keep a skill pointing at a mount
// that is no longer there.
func TestComposeGroupPrompt_PrunesIntrospectionWhenEventsRemoved(t *testing.T) {
	home := t.TempDir()
	if err := composeGroupPrompt(home, false, true); err != nil {
		t.Fatalf("compose with events: %v", err)
	}
	if _, err := os.Readlink(filepath.Join(home, "skills", "introspection")); err != nil {
		t.Fatalf("expected introspection after events compose: %v", err)
	}
	if err := composeGroupPrompt(home, false, false); err != nil {
		t.Fatalf("compose without events: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, "skills", "introspection")); !os.IsNotExist(err) {
		t.Fatalf("introspection should be pruned after recompose without events; lstat err = %v", err)
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

// TestComposeGroupPrompt_EntryPointBodyGolden pins the EXACT byte content of the
// composed entry-point CLAUDE.md across every mount combination. The refactor that
// renders composition from a declarative spec must keep this output byte-identical;
// this golden test is the regression guard for that contract.
func TestComposeGroupPrompt_EntryPointBodyGolden(t *testing.T) {
	const header = "<!-- Composed at launch by goclaw - do not edit. -->\n@./.claude-shared.md\n"
	cases := []struct {
		name          string
		vault, events bool
		want          string
	}{
		{"no_vault_no_events", false, false, header},
		{"vault_no_events", true, false, header + "@" + vaultCriticalFactsPath + "\n"},
		{"no_vault_events", false, true, header},
		{"vault_events", true, true, header + "@" + vaultCriticalFactsPath + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if err := composeGroupPrompt(home, tc.vault, tc.events); err != nil {
				t.Fatalf("compose: %v", err)
			}
			body, err := os.ReadFile(filepath.Join(home, "CLAUDE.md"))
			if err != nil {
				t.Fatalf("read composed CLAUDE.md: %v", err)
			}
			if string(body) != tc.want {
				t.Fatalf("entry-point body mismatch.\n got: %q\nwant: %q", body, tc.want)
			}
		})
	}
}

// TestComposeGroupPrompt_SkillSetGolden pins exactly which skills are linked for
// each mount combination, so the spec-driven refactor cannot silently add, drop, or
// rename a skill.
func TestComposeGroupPrompt_SkillSetGolden(t *testing.T) {
	cases := []struct {
		name          string
		vault, events bool
		want          map[string]string // skill name -> container target
	}{
		{"base", false, false, map[string]string{
			"coding": skillsContainerBase + "/coding",
		}},
		{"vault", true, false, map[string]string{
			"coding":    skillsContainerBase + "/coding",
			"librarian": vaultLibrarianSkillPath,
		}},
		{"events", false, true, map[string]string{
			"coding":        skillsContainerBase + "/coding",
			"introspection": introspectionSkillPath,
		}},
		{"vault_events", true, true, map[string]string{
			"coding":        skillsContainerBase + "/coding",
			"librarian":     vaultLibrarianSkillPath,
			"introspection": introspectionSkillPath,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if err := composeGroupPrompt(home, tc.vault, tc.events); err != nil {
				t.Fatalf("compose: %v", err)
			}
			got := readSkillLinks(t, filepath.Join(home, "skills"))
			if len(got) != len(tc.want) {
				t.Fatalf("skill set size mismatch: got %v want %v", got, tc.want)
			}
			for name, target := range tc.want {
				if got[name] != target {
					t.Fatalf("skill %q target = %q, want %q (full got: %v)", name, got[name], target, got)
				}
			}
		})
	}
}

// readSkillLinks returns the symlink name -> target map under skillsDir.
func readSkillLinks(t *testing.T, skillsDir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		t.Fatalf("read skills dir: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		tgt, err := os.Readlink(filepath.Join(skillsDir, e.Name()))
		if err != nil {
			continue // not a symlink
		}
		out[e.Name()] = tgt
	}
	return out
}

// TestMigrateClaudeHomeLayout_MovesLegacyRootIntoDotClaude: a dir that holds the old
// layout (compose artifacts at the root) is migrated so those artifacts live under
// .claude/, preserving session history on upgrade.
func TestMigrateClaudeHomeLayout_MovesLegacyRootIntoDotClaude(t *testing.T) {
	home := t.TempDir()
	// Legacy: CLAUDE.md + skills/ + a session history file at the ROOT.
	mustWrite(t, filepath.Join(home, "CLAUDE.md"), "old")
	if err := os.MkdirAll(filepath.Join(home, "skills"), 0o777); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(home, "history.jsonl"), "session")

	if err := migrateClaudeHomeLayout(home); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Root artifacts moved under .claude/.
	for _, name := range []string{"CLAUDE.md", "skills", "history.jsonl"} {
		if _, err := os.Stat(filepath.Join(home, claudeDotDirName, name)); err != nil {
			t.Errorf("%q should have moved under .claude/: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(home, name)); !os.IsNotExist(err) {
			t.Errorf("%q should no longer be at the root: %v", name, err)
		}
	}
}

// TestMigrateClaudeHomeLayout_LeavesNewLayoutAlone: a dir that already holds the new
// layout (a HOME-root state file and a .claude/ subdir) must NOT be touched - moving
// ~/.claude.json into .claude/ would lose the CLI state.
func TestMigrateClaudeHomeLayout_LeavesNewLayoutAlone(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, claudeDotDirName), 0o777); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(home, ".claude.json"), "{}")

	if err := migrateClaudeHomeLayout(home); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// ~/.claude.json stays at the HOME root.
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err != nil {
		t.Errorf("~/.claude.json must remain at HOME root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, claudeDotDirName, ".claude.json")); !os.IsNotExist(err) {
		t.Errorf("~/.claude.json must NOT be moved into .claude/")
	}
}

// TestMigrateClaudeHomeLayout_FreshAndIdempotent: a fresh dir is a no-op, and running
// the migration twice is safe.
func TestMigrateClaudeHomeLayout_FreshAndIdempotent(t *testing.T) {
	home := t.TempDir()
	if err := migrateClaudeHomeLayout(home); err != nil {
		t.Fatalf("migrate fresh: %v", err)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("fresh dir should stay empty, got %v", entries)
	}
	// Legacy then migrate twice: second run is a no-op (it sees .claude and returns).
	mustWrite(t, filepath.Join(home, "CLAUDE.md"), "old")
	if err := migrateClaudeHomeLayout(home); err != nil {
		t.Fatalf("migrate legacy: %v", err)
	}
	if err := migrateClaudeHomeLayout(home); err != nil {
		t.Fatalf("migrate idempotent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, claudeDotDirName, "CLAUDE.md")); err != nil {
		t.Errorf("migrated CLAUDE.md should remain under .claude/: %v", err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
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
