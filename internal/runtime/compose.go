package runtime

import (
	"fmt"
	"os"
	"path/filepath"
)

// CLAUDE.md composition for an agent group, mirroring NanoClaw's
// claude-md-compose.ts + container-runner.syncSkillSymlinks.
//
// The agent's system prompt is built from two layers, composed fresh on every
// launch so the inputs are deterministic and stale links are pruned:
//
//   - a generic agent-first BASE, baked into the image at /app/CLAUDE.md
//   - SKILLS the model auto-invokes by description: the coding skill (always,
//     baked at /app/skills/coding), the librarian skill (only when a vault is
//     mounted, from /vault/.claude/skills/librarian), and the introspection skill
//     (only when the event log is mounted, baked at /app/skills/introspection)
//
// We write, into the group's claude-home dir (mounted at the container's
// $HOME/.claude):
//
//   CLAUDE.md             an imports-only entry point: @./.claude-shared.md
//   .claude-shared.md     symlink -> /app/CLAUDE.md          (the base)
//   skills/coding         symlink -> /app/skills/coding      (always)
//   skills/librarian      symlink -> /vault/.claude/skills/librarian (if vault)
//   skills/introspection  symlink -> /app/skills/introspection (if event log mounted)
//
// The symlink targets are CONTAINER paths: they dangle on the host but resolve
// inside the container, where /app and /vault are mounted. The CLI discovers
// skills at the standard ~/.claude/skills/ location, so no extra flags are
// needed; it auto-invokes one when a task matches its description.

// Container-side paths the symlinks point at (valid in the container, not host).
const (
	baseClaudeMdContainerPath = "/app/CLAUDE.md"
	skillsContainerBase       = "/app/skills"
	vaultLibrarianSkillPath   = vaultMountPath + "/.claude/skills/librarian"
	// introspectionSkillPath is the baked-in introspection skill. Unlike librarian
	// (which lives in the vault), it ships in the image, so its target is under
	// /app/skills like coding; it is linked only when the event log is mounted.
	introspectionSkillPath = skillsContainerBase + "/introspection"
	// vaultCriticalFactsPath is the always-load L0 facts file (brief §11). When a
	// vault is mounted we import it directly into the entry-point CLAUDE.md so the
	// owner/timezone/purpose facts are present on EVERY turn, not only when the
	// agent decides to open the librarian skill.
	vaultCriticalFactsPath = vaultMountPath + "/CRITICAL_FACTS.md"
)

// composedClaudeMdName is the entry-point prompt file written into claude-home;
// the runner points --system-prompt-file at its in-container path.
const composedClaudeMdName = "CLAUDE.md"

// composeGroupPrompt writes the imports-only CLAUDE.md and syncs the skill
// symlinks into claudeHome (the host dir mounted at the container's
// $HOME/.claude). vaultMounted toggles the librarian skill; eventsMounted toggles
// the introspection skill. It is deterministic and idempotent: re-running with the
// same inputs yields the same files, and skills no longer desired are unlinked.
// Called on every launch.
func composeGroupPrompt(claudeHome string, vaultMounted, eventsMounted bool) error {
	if err := os.MkdirAll(claudeHome, 0o777); err != nil {
		return fmt.Errorf("compose: create claude home %q: %w", claudeHome, err)
	}

	// .claude-shared.md -> /app/CLAUDE.md (the base prompt).
	if err := syncSymlink(filepath.Join(claudeHome, ".claude-shared.md"), baseClaudeMdContainerPath); err != nil {
		return err
	}

	// Skill symlinks under claudeHome/skills/. coding is always present; librarian
	// only when a vault is mounted; introspection only when the event log is mounted.
	// Anything else is pruned, so unmounting a source removes its link (e.g. dropping
	// to multi-group, which un-gates the events mount, removes introspection).
	desired := map[string]string{
		"coding": skillsContainerBase + "/coding",
	}
	if vaultMounted {
		desired["librarian"] = vaultLibrarianSkillPath
	}
	if eventsMounted {
		desired["introspection"] = introspectionSkillPath
	}
	if err := syncSkillSymlinks(filepath.Join(claudeHome, "skills"), desired); err != nil {
		return err
	}

	// The entry point imports only: the base, then (implicitly) the skills the
	// CLI discovers from skills/. Mirrors NanoClaw's imports-only CLAUDE.md. When
	// a vault is mounted we also import its CRITICAL_FACTS.md so the L0 facts load
	// on every turn regardless of whether the librarian skill is invoked. The
	// import target is a container path (/vault/...), dangling on the host but
	// resolved inside the container, like the skill symlinks above.
	body := "<!-- Composed at launch by goclaw - do not edit. -->\n@./.claude-shared.md\n"
	if vaultMounted {
		body += "@" + vaultCriticalFactsPath + "\n"
	}
	if err := writeAtomic(filepath.Join(claudeHome, composedClaudeMdName), body); err != nil {
		return err
	}
	return nil
}

// syncSkillSymlinks makes skillsDir contain exactly one symlink per desired
// skill (name -> container target), removing any symlink not in desired. Targets
// are container paths, so they are written without checking existence on the
// host (they dangle here, resolve in the container).
func syncSkillSymlinks(skillsDir string, desired map[string]string) error {
	if err := os.MkdirAll(skillsDir, 0o777); err != nil {
		return fmt.Errorf("compose: create skills dir %q: %w", skillsDir, err)
	}
	// Prune symlinks not desired.
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return fmt.Errorf("compose: read skills dir %q: %w", skillsDir, err)
	}
	for _, e := range entries {
		if _, keep := desired[e.Name()]; keep {
			continue
		}
		info, lerr := os.Lstat(filepath.Join(skillsDir, e.Name()))
		if lerr != nil || info.Mode()&os.ModeSymlink == 0 {
			continue // only remove our own symlinks, never real files
		}
		if err := os.Remove(filepath.Join(skillsDir, e.Name())); err != nil {
			return fmt.Errorf("compose: prune skill link %q: %w", e.Name(), err)
		}
	}
	// Create/refresh desired symlinks.
	for name, target := range desired {
		if err := syncSymlink(filepath.Join(skillsDir, name), target); err != nil {
			return err
		}
	}
	return nil
}

// syncSymlink ensures linkPath is a symlink pointing at target, replacing any
// existing link with a different target. The target may be a container path that
// does not exist on the host (a dangling link here, valid in the container), so
// we never stat the target.
func syncSymlink(linkPath, target string) error {
	if current, err := os.Readlink(linkPath); err == nil {
		if current == target {
			return nil
		}
		if err := os.Remove(linkPath); err != nil {
			return fmt.Errorf("compose: replace symlink %q: %w", linkPath, err)
		}
	} else if !os.IsNotExist(err) {
		// A non-symlink may sit here; remove it so we can place the link.
		if _, lerr := os.Lstat(linkPath); lerr == nil {
			if rerr := os.Remove(linkPath); rerr != nil {
				return fmt.Errorf("compose: clear path %q: %w", linkPath, rerr)
			}
		}
	}
	if err := os.Symlink(target, linkPath); err != nil {
		return fmt.Errorf("compose: symlink %q -> %q: %w", linkPath, target, err)
	}
	return nil
}

// writeAtomic writes content to filePath via a temp file + rename so a reader
// never sees a partial file.
func writeAtomic(filePath, content string) error {
	tmp := filePath + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("compose: write %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, filePath); err != nil {
		return fmt.Errorf("compose: rename %q -> %q: %w", tmp, filePath, err)
	}
	return nil
}
