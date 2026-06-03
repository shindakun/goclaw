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
