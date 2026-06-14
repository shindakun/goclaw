package main

import (
	"strings"
	"testing"
)

// TestHarnessInvariants_GoldenText pins the EXACT rendered text of the harness
// invariants against the strings that were previously inlined in query(), so the
// move to spec-driven harness data is provably byte-identical. If the wording must
// change, change it deliberately and update these goldens.
func TestHarnessInvariants_GoldenText(t *testing.T) {
	const now = "2026-06-14 10:00 UTC"
	vals := map[string]string{"vault_dir": vaultDir, "work_dir": workDir, "now": now}

	wantVault := "Your knowledge vault is mounted at the ABSOLUTE path " + vaultDir +
		". Always read and write vault notes under " + vaultDir +
		" (e.g. " + vaultDir + "/wiki/tasks/, " + vaultDir + "/index.md, " + vaultDir +
		"/log.md). Your current working directory (" + workDir +
		") is scratch space for clones and temp files only; the vault is NOT there. " +
		"When the vault manual says a path like \"wiki/tasks/\", it means " + vaultDir + "/wiki/tasks/."

	wantTime := "The current date and time is " + now +
		" (24-hour clock). Use THIS as 'now' for any timestamp you write - " +
		"log lines, lease_until, handoff notes - in YYYY-MM-DD HH:MM form. " +
		"Never guess the time, and never write an hour outside 00-23 (midnight " +
		"is 00:00 of the next day, not 24:00)."

	// With a vault: vault-path block first (it is declared before the time block and
	// both are TierMust), then the time block.
	gotVault := harnessSpec().RenderInvariants(true, vals)
	if len(gotVault) != 2 {
		t.Fatalf("expected 2 invariants with a vault, got %d: %#v", len(gotVault), gotVault)
	}
	if gotVault[0] != wantVault {
		t.Fatalf("vault-path text mismatch.\n got: %q\nwant: %q", gotVault[0], wantVault)
	}
	if gotVault[1] != wantTime {
		t.Fatalf("time text mismatch.\n got: %q\nwant: %q", gotVault[1], wantTime)
	}

	// Without a vault: only the time block, identical text.
	gotNoVault := harnessSpec().RenderInvariants(false, vals)
	if len(gotNoVault) != 1 || gotNoVault[0] != wantTime {
		t.Fatalf("no-vault render = %#v, want exactly [time]", gotNoVault)
	}

	// No placeholder leaks through in either rendering.
	for _, s := range append(gotVault, gotNoVault...) {
		if strings.Contains(s, "{{") {
			t.Fatalf("unsubstituted placeholder in rendered invariant: %q", s)
		}
	}
}
