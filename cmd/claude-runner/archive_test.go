package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A realistic-ish transcript: user line (string content), assistant line (block
// array content), a tool-use row that must be skipped, and a system row.
const sampleTranscript = `{"type":"user","message":{"role":"user","content":"what is 2+2"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"It is 4."}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{}}]}}
{"type":"system","subtype":"init","message":{"role":"system","content":"boot"}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"thanks"}]}}
`

func TestParseTranscript_ExtractsTextTurnsOnly(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(p, []byte(sampleTranscript), 0o644); err != nil {
		t.Fatal(err)
	}
	msgs, err := parseTranscript(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// user "what is 2+2", assistant "It is 4.", user "thanks" — tool-use + system skipped.
	if len(msgs) != 3 {
		t.Fatalf("expected 3 text turns, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].role != "user" || msgs[0].text != "what is 2+2" {
		t.Fatalf("turn 0 = %+v", msgs[0])
	}
	if msgs[1].role != "assistant" || msgs[1].text != "It is 4." {
		t.Fatalf("turn 1 = %+v", msgs[1])
	}
	if msgs[2].text != "thanks" {
		t.Fatalf("turn 2 = %+v", msgs[2])
	}
}

func TestArchiveTranscript_WritesMarkdown(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	tp := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(tp, []byte(sampleTranscript), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 3, 14, 5, 0, 0, time.UTC)
	dest, err := archiveTranscript(tp, "my session", now)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if dest == "" {
		t.Fatal("expected a written archive path")
	}
	// Lands in $HOME/.claude/conversations/ with a dated, slugged name.
	if !strings.Contains(dest, filepath.Join(home, ".claude", "conversations")) {
		t.Fatalf("archive not in conversations dir: %s", dest)
	}
	if !strings.HasSuffix(dest, "2026-06-03-my-session.md") {
		t.Fatalf("unexpected archive filename: %s", dest)
	}
	body, _ := os.ReadFile(dest)
	s := string(body)
	for _, want := range []string{"# my session", "**User**: what is 2+2", "**Assistant**: It is 4.", "Archived: 2026-06-03 14:05"} {
		if !strings.Contains(s, want) {
			t.Fatalf("archive missing %q; got:\n%s", want, s)
		}
	}
}

func TestArchiveTranscript_EmptyTranscriptNoFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	tp := filepath.Join(dir, "empty.jsonl")
	// Only tool-use / system rows: nothing text to archive.
	if err := os.WriteFile(tp, []byte(`{"type":"system","message":{"content":"x"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest, err := archiveTranscript(tp, "x", time.Now())
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if dest != "" {
		t.Fatalf("expected no archive for an empty transcript, got %s", dest)
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"My Session!":            "my-session",
		"  spaces  and--dashes ": "spaces-and-dashes",
		"":                       "",
		"UPPER_case 123":         "upper-case-123",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractText_StringAndBlocks(t *testing.T) {
	if got := extractText([]byte(`"hello"`)); got != "hello" {
		t.Errorf("string content: got %q", got)
	}
	blocks := `[{"type":"text","text":"a"},{"type":"tool_use"},{"type":"text","text":"b"}]`
	if got := extractText([]byte(blocks)); got != "a\nb" {
		t.Errorf("block content: got %q", got)
	}
	if got := extractText([]byte(`[{"type":"image"}]`)); got != "" {
		t.Errorf("non-text blocks should yield empty, got %q", got)
	}
}
