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
		return fmt.Errorf("usage: goclaw vault init [dir]")
	}
	switch args[0] {
	case "init":
		return vaultInit(args[1:])
	default:
		return fmt.Errorf("unknown vault subcommand %q (try: init)", args[0])
	}
}

// vaultInit installs the embedded vault template into a directory (default
// ~/Vault). Idempotent: existing files are left untouched.
func vaultInit(args []string) error {
	dir := ""
	if len(args) > 0 {
		dir = args[0]
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home: %w", err)
		}
		dir = filepath.Join(home, "Vault")
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
