package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/shindakun/goclaw/internal/vaultinit"
)

// runVault handles `goclaw vault <subcommand>`.
func runVault(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage:\n" +
			"  goclaw vault init [dir]          create a vault (idempotent; never overwrites)\n" +
			"  goclaw vault sync [dir] [--dry-run]  refresh goclaw-owned files after a goclaw upgrade")
	}
	switch args[0] {
	case "init":
		return vaultInit(args[1:])
	case "sync":
		return vaultSync(args[1:])
	default:
		return fmt.Errorf("unknown vault subcommand %q (try: init, sync)", args[0])
	}
}

// defaultVaultDir resolves the vault directory: an explicit arg (first non-flag), else
// GOCLAW_VAULT_DIR, else ~/Vault.
func defaultVaultDir(arg string) (string, error) {
	if arg != "" {
		return arg, nil
	}
	if v := os.Getenv("GOCLAW_VAULT_DIR"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(home, "Vault"), nil
}

// vaultSync refreshes the goclaw-OWNED operating-contract files in an existing vault from the
// current embedded template (after a goclaw upgrade). It overwrites ONLY those files and
// never user content; --dry-run reports what would change.
func vaultSync(args []string) error {
	dryRun := false
	dir := ""
	for _, a := range args {
		switch a {
		case "--dry-run", "-n":
			dryRun = true
		default:
			if dir == "" {
				dir = a
			}
		}
	}
	resolved, err := defaultVaultDir(dir)
	if err != nil {
		return err
	}

	res, err := vaultinit.Sync(resolved, dryRun)
	if err != nil {
		return err
	}

	verb := "Synced"
	if res.DryRun {
		verb = "Would sync"
	}
	fmt.Printf("%s goclaw-owned files in %s\n", verb, res.Dir)
	for _, f := range res.Updated {
		fmt.Printf("  updated:   %s\n", f)
	}
	for _, f := range res.Added {
		fmt.Printf("  added:     %s\n", f)
	}
	for _, f := range res.Same {
		fmt.Printf("  unchanged: %s\n", f)
	}
	if len(res.Updated) == 0 && len(res.Added) == 0 {
		fmt.Println("  everything already current.")
	}
	if len(res.Backups) > 0 {
		fmt.Printf("  backups written (%d): each replaced file kept as <file>.bak\n", len(res.Backups))
	}
	if res.DryRun && (len(res.Updated) > 0 || len(res.Added) > 0) {
		fmt.Println("  (dry run, nothing written; re-run without --dry-run to apply)")
	}
	fmt.Println("\nOnly goclaw's operating-contract files are touched (the agent's rulebook).")
	fmt.Println("Your notes, index.md, log.md, and CRITICAL_FACTS.md are never modified.")
	return nil
}

// vaultInit installs the embedded vault template into a directory (default
// ~/Vault). Idempotent: existing files are left untouched.
func vaultInit(args []string) error {
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	dir, err := defaultVaultDir(arg)
	if err != nil {
		return err
	}

	res, err := vaultinit.Install(dir)
	if err != nil {
		return err
	}

	fmt.Printf("Vault ready at %s\n", res.Dir)
	fmt.Printf("  %d files written, %d already present\n", len(res.Written), len(res.Skipped))
	if res.GitInited {
		fmt.Println("  initialized a git repo (the vault's revert safety net)")
	}
	fmt.Println()
	fmt.Println("Next:")
	fmt.Println("  1. Fill in CRITICAL_FACTS.md (owner, purpose, timezone).")
	fmt.Printf("  2. Set GOCLAW_VAULT_DIR=%s and restart goclaw to mount it.\n", res.Dir)
	fmt.Println("  3. Open the folder in Obsidian to browse/edit alongside the agent.")
	return nil
}
