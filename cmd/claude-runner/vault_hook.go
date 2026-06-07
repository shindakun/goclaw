package main

import (
	"context"
	"encoding/json"

	claude "github.com/shindakun/agent-sdk-go"
)

// vaultFirstReminderText is injected into the agent's context on EVERY turn (when a vault is
// mounted) to keep the vault-first discipline at decision time, where a buried system-prompt
// line gets scrolled past. It mirrors the durable-knowledge routing in container/CLAUDE.md:
// vault knowledge goes in the vault as librarian notes, auto-memory is only thin operational
// pointers, and a fact in both stores is a bug.
const vaultFirstReminderText = "VAULT-FIRST (/vault is mounted): before answering about the " +
	"user or their work, check the vault. When you learn a DURABLE fact (people, work, tools, " +
	"projects, decisions), file it as a /vault note via the librarian skill, NOT auto-memory. " +
	"Auto-memory holds only thin operational pointers; a fact in both stores is a bug."

// userPromptSubmitContext is the hookSpecificOutput shape the CLI reads for a
// UserPromptSubmit hook: additionalContext is the documented field that is injected into the
// MODEL's context for that turn (as opposed to SystemMessage, which is user-facing).
type userPromptSubmitContext struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// vaultFirstReminder is the UserPromptSubmit hook callback: it injects the vault-first
// reminder into the agent's context each turn. Registered only when a vault is mounted (see
// main). It never blocks the prompt; on any marshal error it is a silent no-op so a hook
// problem can never break message handling.
func (r *runner) vaultFirstReminder(_ context.Context, _ json.RawMessage, _ string) (claude.HookOutput, error) {
	payload, err := json.Marshal(userPromptSubmitContext{
		HookEventName:     "UserPromptSubmit",
		AdditionalContext: vaultFirstReminderText,
	})
	if err != nil {
		r.log.Warn("vault-first reminder: marshal", "err", err)
		return claude.HookOutput{}, nil
	}
	return claude.HookOutput{
		HookSpecificOutput: payload,
		SuppressOutput:     true,
	}, nil
}
