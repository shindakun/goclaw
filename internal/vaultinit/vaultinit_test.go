package vaultinit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstall_FreshDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	res, err := Install(dir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(res.Written) == 0 {
		t.Fatal("expected files written into a fresh vault")
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("fresh install should skip nothing, skipped %v", res.Skipped)
	}
	// The template ships a CLAUDE.md system prompt; it must land.
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatalf("CLAUDE.md not installed: %v", err)
	}
	// Result.Dir is absolute.
	if !filepath.IsAbs(res.Dir) {
		t.Fatalf("Result.Dir not absolute: %q", res.Dir)
	}
}

// Install is idempotent: a second run over the same dir overwrites nothing and
// reports every file as skipped, writing none.
func TestInstall_Idempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	first, err := Install(dir)
	if err != nil {
		t.Fatalf("first Install: %v", err)
	}
	second, err := Install(dir)
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if len(second.Written) != 0 {
		t.Fatalf("second install wrote files: %v", second.Written)
	}
	if len(second.Skipped) != len(first.Written) {
		t.Fatalf("skipped %d on rerun, want %d (all first-run files)",
			len(second.Skipped), len(first.Written))
	}
}

// An existing file must never be clobbered; its content survives Install.
func TestInstall_DoesNotOverwrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(custom, []byte("MY CUSTOM PROMPT"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Install(dir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	got, err := os.ReadFile(custom)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "MY CUSTOM PROMPT" {
		t.Fatalf("Install overwrote an existing file; content = %q", got)
	}
	// And it was reported as skipped, not written.
	var skipped bool
	for _, s := range res.Skipped {
		if s == "CLAUDE.md" {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("CLAUDE.md not in Skipped: %v", res.Skipped)
	}
}

// On a fresh non-repo dir, Install runs git init (when git is present).
func TestInstall_GitInit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	res, err := Install(dir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.GitInited {
		// git was available and initialized: a .git dir must exist.
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			t.Fatalf("GitInited true but no .git: %v", err)
		}
	} else {
		t.Log("git not available in this environment; GitInited=false is acceptable")
	}
}

// Installing into an existing git repo must not re-init it.
func TestInstall_ExistingRepoNotReinited(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Install(dir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.GitInited {
		t.Fatal("GitInited should be false when dir is already a repo")
	}
}

// Sync refreshes ONLY the goclaw-owned contract files and NEVER user content. This is the
// safety property: a goclaw upgrade can push new prompt/skill text without clobbering the
// user's notes/index/log.
func TestSync_UpdatesOwnedNotUser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	if _, err := Install(dir); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Drift an OWNED file (the librarian skill) and a USER file (index.md).
	skill := filepath.Join(dir, ".claude/skills/librarian/SKILL.md")
	if err := os.WriteFile(skill, []byte("OLD DRIFTED SKILL"), 0o644); err != nil {
		t.Fatal(err)
	}
	userIndex := filepath.Join(dir, "index.md")
	const userContent = "MY OWN INDEX - do not touch\n- [[some-note]]\n"
	if err := os.WriteFile(userIndex, []byte(userContent), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Sync(dir, false)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// The owned skill was updated back to the template.
	if !contains(res.Updated, ".claude/skills/librarian/SKILL.md") {
		t.Fatalf("SKILL.md not reported updated: %+v", res)
	}
	got, _ := os.ReadFile(skill)
	if string(got) == "OLD DRIFTED SKILL" {
		t.Fatal("SKILL.md was not refreshed from the template")
	}
	want, _ := vaultTemplate.ReadFile(templateRoot + "/.claude/skills/librarian/SKILL.md")
	if string(got) != string(want) {
		t.Fatal("SKILL.md does not match the template after sync")
	}
	// A .bak of the old content was kept (non-destructive).
	bak, err := os.ReadFile(skill + ".bak")
	if err != nil || string(bak) != "OLD DRIFTED SKILL" {
		t.Fatalf("expected a .bak with the old content, got err=%v content=%q", err, bak)
	}

	// The USER file is byte-for-byte untouched, this is the load-bearing guarantee.
	if cur, _ := os.ReadFile(userIndex); string(cur) != userContent {
		t.Fatalf("index.md (user content) was modified by sync: %q", cur)
	}
	// And sync never reports a user file in any bucket.
	for _, f := range append(append(res.Updated, res.Added...), res.Same...) {
		if f == "index.md" || f == "log.md" || f == "CRITICAL_FACTS.md" {
			t.Fatalf("sync touched a user-owned file: %s", f)
		}
	}
}

// A dry run reports what would change but writes nothing.
func TestSync_DryRunWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	if _, err := Install(dir); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(dir, ".claude/skills/librarian/SKILL.md")
	if err := os.WriteFile(skill, []byte("DRIFTED"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Sync(dir, true)
	if err != nil {
		t.Fatalf("Sync dry-run: %v", err)
	}
	if !res.DryRun || !contains(res.Updated, ".claude/skills/librarian/SKILL.md") {
		t.Fatalf("dry-run should report the drifted file as would-update: %+v", res)
	}
	// Nothing was written: the file still holds the drifted content and no .bak exists.
	if cur, _ := os.ReadFile(skill); string(cur) != "DRIFTED" {
		t.Fatal("dry-run modified the file")
	}
	if _, err := os.Stat(skill + ".bak"); err == nil {
		t.Fatal("dry-run wrote a .bak")
	}
}

// Sync errors clearly when the vault does not exist.
func TestSync_MissingVault(t *testing.T) {
	if _, err := Sync(filepath.Join(t.TempDir(), "nope"), false); err == nil {
		t.Fatal("expected an error syncing a non-existent vault")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
