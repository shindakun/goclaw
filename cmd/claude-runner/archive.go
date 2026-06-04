package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	claude "github.com/shindakun/agent-sdk-go"
)

// Conversation archival (mirrors NanoClaw's archiveTranscriptFile).
//
// Before a transcript is compacted away (PreCompact hook) or rotated aside
// (rotate.go), render it to a readable markdown file under conversations/ so the
// agent can grep its own history to recall context that has left the live
// session. This is BASE memory: it is always on, lives in the persisted
// claude-home, and does NOT depend on a vault. A mounted vault is an additional
// memory surface on top, never a replacement for this.

// conversationsDir is where rendered transcripts land: $HOME/.claude/conversations/.
// It sits next to the CLI's projects/ in the per-group claude-home the host
// persists, so archives survive container churn. Vault-independent by design.
func conversationsDir() string {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/agent"
	}
	if base := os.Getenv("CLAUDE_CONFIG_DIR"); base != "" {
		return filepath.Join(base, "conversations")
	}
	return filepath.Join(home, ".claude", "conversations")
}

// parsedMsg is one user/assistant turn extracted from a transcript .jsonl.
type parsedMsg struct {
	role string // "user" | "assistant"
	text string
}

// parseTranscript reads a CLI transcript .jsonl and pulls out the user and
// assistant text turns, skipping tool calls, system rows, and non-text content.
// Best-effort: malformed lines are skipped, not fatal.
func parseTranscript(path string) ([]parsedMsg, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var out []parsedMsg
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // transcripts can have long lines
	for sc.Scan() {
		var row struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			continue
		}
		if row.Type != "user" && row.Type != "assistant" {
			continue
		}
		text := extractText(row.Message.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		role := row.Message.Role
		if role == "" {
			role = row.Type
		}
		out = append(out, parsedMsg{role: role, text: text})
	}
	return out, sc.Err()
}

// extractText pulls plain text out of a transcript content field, which is
// either a bare string or an array of content blocks ({type:"text", text:...}).
// Tool-use/result and image blocks are skipped.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" && blk.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// formatTranscriptMarkdown renders parsed turns to a readable markdown document.
// Long turns are truncated so an archive stays greppable rather than huge.
func formatTranscriptMarkdown(msgs []parsedMsg, title string, now time.Time) string {
	if title == "" {
		title = "Conversation"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\nArchived: %s\n\n---\n\n", title, now.Format("2006-01-02 15:04"))
	for _, m := range msgs {
		sender := "Assistant"
		if m.role == "user" {
			sender = "User"
		}
		text := m.text
		if len(text) > 2000 {
			text = text[:2000] + "..."
		}
		fmt.Fprintf(&b, "**%s**: %s\n\n", sender, text)
	}
	return b.String()
}

// archiveTranscript renders the transcript at path into conversations/ as dated
// markdown. titleHint (e.g. a short summary slug) names the file; falls back to
// the session id. Returns the written path, or "" if there was nothing to
// archive (empty/unreadable transcript). Errors are returned but treated as
// non-fatal by callers - losing an archive must never break a reply.
func archiveTranscript(path, titleHint string, now time.Time) (string, error) {
	msgs, err := parseTranscript(path)
	if err != nil {
		return "", err
	}
	if len(msgs) == 0 {
		return "", nil
	}
	dir := conversationsDir()
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	slug := slugify(titleHint)
	if slug == "" {
		slug = "conversation"
	}
	filename := now.Format("2006-01-02") + "-" + slug + ".md"
	dest := filepath.Join(dir, filename)
	body := formatTranscriptMarkdown(msgs, titleHint, now)
	if err := os.WriteFile(dest, []byte(body), 0o644); err != nil {
		return "", err
	}
	return dest, nil
}

// preCompactArchive is the PreCompact hook callback: the CLI is about to compact
// the conversation, so archive the current transcript first. The payload embeds
// BaseHookInput, which carries the transcript path + session id. Always returns a
// zero (no-op) HookOutput - archival must never block or alter the compaction.
func (r *runner) preCompactArchive(_ context.Context, input json.RawMessage, _ string) (claude.HookOutput, error) {
	var in claude.PreCompactHookInput
	if err := json.Unmarshal(input, &in); err != nil {
		r.log.Warn("pre-compact archive: decode payload", "err", err)
		return claude.HookOutput{}, nil
	}
	if in.TranscriptPath == "" {
		return claude.HookOutput{}, nil
	}
	hint := in.SessionID
	if hint == "" {
		hint = "compacted"
	}
	if dest, err := archiveTranscript(in.TranscriptPath, "compacted-"+hint, time.Now()); err != nil {
		r.log.Warn("pre-compact archive failed", "path", in.TranscriptPath, "err", err)
	} else if dest != "" {
		r.log.Info("archived conversation before compact", "to", dest)
	}
	return claude.HookOutput{}, nil
}

// slugify reduces a hint to a short filesystem-safe slug.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
		if b.Len() >= 50 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}
