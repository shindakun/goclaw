package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The vault-first hook must return hookSpecificOutput whose additionalContext carries the
// vault-first reminder and the correct event name, the documented shape the CLI injects into
// the model's context for a UserPromptSubmit hook. This is the per-turn mechanism that keeps
// the agent routing durable knowledge to the vault instead of auto-memory.
func TestVaultFirstReminder_InjectsAdditionalContext(t *testing.T) {
	r := &runner{vaultMounted: true, log: quietLogger()}

	out, err := r.vaultFirstReminder(context.Background(), nil, "")
	if err != nil {
		t.Fatalf("vaultFirstReminder: %v", err)
	}
	if len(out.HookSpecificOutput) == 0 {
		t.Fatal("no HookSpecificOutput; nothing would be injected into context")
	}

	var got userPromptSubmitContext
	if err := json.Unmarshal(out.HookSpecificOutput, &got); err != nil {
		t.Fatalf("HookSpecificOutput is not valid JSON: %v", err)
	}
	if got.HookEventName != "UserPromptSubmit" {
		t.Fatalf("hookEventName = %q, want UserPromptSubmit (CLI keys context injection on this)", got.HookEventName)
	}
	// The injected text must actually carry the vault-first routing, not be empty/garbled.
	for _, want := range []string{"VAULT-FIRST", "librarian", "NOT auto-memory"} {
		if !strings.Contains(got.AdditionalContext, want) {
			t.Fatalf("additionalContext missing %q: %q", want, got.AdditionalContext)
		}
	}
	if !out.SuppressOutput {
		t.Error("hook stdout should be suppressed from the transcript")
	}
}
