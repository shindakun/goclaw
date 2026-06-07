// Package vaultinit installs the embedded vault template into a target
// directory, creating a ready-to-use knowledge vault (brief §11). It is
// idempotent and never overwrites a file that already exists, so re-running it
// over a live vault is safe (it only fills in missing template files).
package vaultinit

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// vaultTemplate holds the starter knowledge-vault layout (brief §11). The
// `all:` prefix includes dotfiles like .gitkeep that a bare glob would skip.
//
//go:embed all:template
var vaultTemplate embed.FS

// templateRoot is the path prefix inside vaultTemplate.
const templateRoot = "template"

// Result reports what an install did.
type Result struct {
	Dir       string
	Written   []string // files created this run
	Skipped   []string // files that already existed (left untouched)
	GitInited bool     // whether `git init` ran (only on a fresh, non-repo dir)
}

// Install writes the embedded vault template into dir. Existing files are left
// untouched (idempotent). If dir is not already a git repo, it runs `git init`
// so the vault gets its safety-net history (brief §11.6); a git failure is not
// fatal.
func Install(dir string) (*Result, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("vaultinit: resolve %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("vaultinit: create %q: %w", abs, err)
	}

	res := &Result{Dir: abs}
	root := templateRoot

	err = fs.WalkDir(vaultTemplate, root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// rel is the path within the vault (strip the "template/" prefix).
		rel := strings.TrimPrefix(p, root)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			return nil // the root dir itself
		}
		dest := filepath.Join(abs, rel)

		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		if _, err := os.Stat(dest); err == nil {
			res.Skipped = append(res.Skipped, rel)
			return nil // never overwrite an existing file
		}
		data, err := vaultTemplate.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read embedded %q: %w", p, err)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return fmt.Errorf("write %q: %w", dest, err)
		}
		res.Written = append(res.Written, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("vaultinit: copy template: %w", err)
	}

	if !isGitRepo(abs) {
		if gitInit(abs) == nil {
			res.GitInited = true
		}
	}
	return res, nil
}

// ownedFiles are the goclaw-AUTHORED operating-contract files inside a vault: the agent's
// rulebook, not user content. `goclaw vault sync` overwrites exactly these from the current
// embedded template so a goclaw upgrade can refresh them, while NEVER touching user-owned
// files (index.md, log.md, CRITICAL_FACTS.md, every wiki/ note), which the template only
// SEEDS and the user then fills. Keeping this an explicit ALLOWLIST is the safety property:
// sync defaults to not touching anything, and only the named files are ever replaced.
var ownedFiles = []string{
	".claude/skills/librarian/SKILL.md", // the librarian discipline
	"CLAUDE.md",                         // the vault's system prompt
}

// SyncResult reports what a Sync did.
type SyncResult struct {
	Dir     string
	Updated []string // owned files replaced because they differed from the template
	Same    []string // owned files already matching the template
	Added   []string // owned files that were missing and got created
	Backups []string // ".bak" copies written before overwriting (empty in dry-run)
	DryRun  bool
}

// Sync refreshes the goclaw-OWNED operating-contract files (ownedFiles) in an existing vault
// from the current embedded template, so a goclaw upgrade can push new prompt/skill text into
// a live vault. It overwrites ONLY ownedFiles and never user content. Before overwriting a
// changed file it writes a "<file>.bak" so a sync is non-destructive. dryRun reports what
// WOULD change without writing anything.
func Sync(dir string, dryRun bool) (*SyncResult, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("vaultinit: resolve %q: %w", dir, err)
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("vaultinit: vault %q does not exist (run `goclaw vault init` first): %w", abs, err)
	}
	res := &SyncResult{Dir: abs, DryRun: dryRun}

	for _, rel := range ownedFiles {
		want, err := vaultTemplate.ReadFile(templateRoot + "/" + rel)
		if err != nil {
			return nil, fmt.Errorf("vaultinit: read embedded %q: %w", rel, err)
		}
		dest := filepath.Join(abs, filepath.FromSlash(rel))
		cur, readErr := os.ReadFile(dest)
		switch {
		case readErr != nil: // missing -> create
			res.Added = append(res.Added, rel)
			if dryRun {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(dest, want, 0o644); err != nil {
				return nil, fmt.Errorf("vaultinit: write %q: %w", rel, err)
			}
		case string(cur) == string(want): // already current
			res.Same = append(res.Same, rel)
		default: // differs -> back up, then overwrite
			res.Updated = append(res.Updated, rel)
			if dryRun {
				continue
			}
			bak := dest + ".bak"
			if err := os.WriteFile(bak, cur, 0o644); err != nil {
				return nil, fmt.Errorf("vaultinit: backup %q: %w", rel, err)
			}
			res.Backups = append(res.Backups, rel+".bak")
			if err := os.WriteFile(dest, want, 0o644); err != nil {
				return nil, fmt.Errorf("vaultinit: write %q: %w", rel, err)
			}
		}
	}
	return res, nil
}

func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// gitInit runs `git init` in dir, ignoring the result if git isn't available.
func gitInit(dir string) error {
	cmd := exec.Command("git", "init")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return err // no git binary; caller just reports GitInited=false
		}
		return err
	}
	return nil
}
