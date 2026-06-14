package main

import "github.com/shindakun/goclaw/internal/agentspec"

// The runner's harness invariants: the always-on instruction blocks appended to the
// system prompt every turn. They live here as DATA (templates with {{placeholders}})
// rather than as string literals inline in the query loop, so the wording is owned in
// one place and the per-turn live values (the current time, the container mount
// paths) are substituted at render time without baking a stale value into anything.
//
// Order is by tier (both are TierMust here); equal-tier invariants keep declared
// order, so the vault-path block precedes the time block, matching the prior inline
// emission order exactly.
//
// Placeholders, filled at query time:
//
//	{{vault_dir}} - the in-container vault mount point
//	{{work_dir}}  - the agent's scratch working directory
//	{{now}}       - the authoritative current time, formatted "2006-01-02 15:04 MST"
var harnessInvariants = []agentspec.Invariant{
	{
		Name:          "vault-path",
		Tier:          agentspec.TierMust,
		RequiresVault: true,
		Text: "Your knowledge vault is mounted at the ABSOLUTE path {{vault_dir}}" +
			". Always read and write vault notes under {{vault_dir}}" +
			" (e.g. {{vault_dir}}/wiki/tasks/, {{vault_dir}}/index.md, {{vault_dir}}" +
			"/log.md). Your current working directory ({{work_dir}}" +
			") is scratch space for clones and temp files only; the vault is NOT there. " +
			"When the vault manual says a path like \"wiki/tasks/\", it means {{vault_dir}}/wiki/tasks/.",
	},
	{
		Name: "authoritative-time",
		Tier: agentspec.TierMust,
		Text: "The current date and time is {{now}}" +
			" (24-hour clock). Use THIS as 'now' for any timestamp you write - " +
			"log lines, lease_until, handoff notes - in YYYY-MM-DD HH:MM form. " +
			"Never guess the time, and never write an hour outside 00-23 (midnight " +
			"is 00:00 of the next day, not 24:00).",
	},
}

// harnessSpec returns the runner's HarnessSpec.
func harnessSpec() agentspec.HarnessSpec {
	return agentspec.HarnessSpec{Invariants: harnessInvariants}
}
