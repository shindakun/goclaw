// Package outscan inspects an agent reply just before it leaves the host, as a
// defense-in-depth layer BENEATH containment, not a replacement for it. The
// container boundary bounds what the agent can REACH; this bounds what the agent
// can SEND. The two failure modes it closes:
//
//   - Exfil through the authorized channel: a poisoned inbound message convinces
//     the agent to paste a secret it legitimately holds (its own env, a mounted
//     vault file, a value it read in-container) into a reply. Delivery
//     authorization checks WHERE a message may go; it never checks WHAT is in it.
//   - Beacon exfil: a markdown image/link to an attacker host carrying a payload
//     in the query string, which renders client-side and leaks on view.
//
// Design constraints, deliberately narrow so this stays deterministic and not a
// heuristic guessing game (the thing that makes pattern-matched inbound filters
// theater):
//
//   - High-precision patterns only. Each match is a recognizable secret SHAPE
//     (private-key armor, known API-key prefixes) or an unambiguous beacon shape,
//     never "this looks instruction-like".
//   - Exact literal needles for THIS container's injected secrets. The host knows,
//     at wire time, the real token values it put in a runner's env (the raw-key
//     fallback path puts a real ANTHROPIC_API_KEY / GH_TOKEN in the container).
//     An exact-substring match for those values is zero-false-positive and catches
//     the precise "agent echoed its own key" case. Supplied per-deployment via
//     WithLiterals; empty by default.
//   - Fail closed at the call site: a nil scanner does not run, but a configured
//     scanner that matches DENIES. The delivery loop treats a block as terminal.
//
// What this is NOT: an inbound prompt-injection scanner. Scanning untrusted INPUT
// with regexes invites bypass and false confidence; containment handles input by
// not trusting it. This scans trusted-path OUTPUT for a small set of things that
// must never leave, which is a tractable, high-precision problem.
package outscan

import (
	"fmt"
	"regexp"
	"strings"
)

// Verdict is the result of scanning one outbound message.
type Verdict struct {
	Block       bool   // true => do not send
	Reason      string // short, safe-to-log cause (a rule label, never the matched secret)
	Replacement string // when non-empty and Block is false, send this redacted text instead
}

// rule is one high-precision secret/beacon pattern. label is logged; it must not
// echo the match. redactable rules are replaced inline rather than hard-blocked.
type rule struct {
	label  string
	re     *regexp.Regexp
	redact bool // true => replace each match with redactMarker and send; false => hard block
}

const redactMarker = "[REDACTED]"

// rules is the curated pattern set. Order does not matter (every rule is checked).
// Each pattern is anchored to a structural shape, not to surrounding prose.
var rules = []rule{
	// Private-key armor. Unambiguous; never a legitimate chat reply. Hard block:
	// a key block is not something to "redact part of" and ship.
	{label: "private_key_block", re: regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP )?PRIVATE KEY-----`), redact: false},

	// Known-prefix API keys / tokens. Redact-and-send: the prefix shape is
	// specific enough to be near-zero false positive, and redacting keeps the
	// conversation flowing if the surrounding text was legitimate.
	{label: "anthropic_key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{20,}`), redact: true},
	{label: "openai_key", re: regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}`), redact: true},
	{label: "github_token", re: regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{20,}`), redact: true},
	{label: "slack_token", re: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`), redact: true},
	{label: "aws_access_key", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), redact: true},
	{label: "stripe_live_key", re: regexp.MustCompile(`\b(?:sk|rk)_live_[A-Za-z0-9]{16,}`), redact: true},
	{label: "google_api_key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`), redact: true},

	// Beacon exfil: a markdown image OR link whose URL carries a query string. The
	// query string is the payload carrier; a bare link without one is not flagged.
	// Hard block: a beacon's whole purpose is the URL, so there is nothing safe to
	// send by redacting only part of it, and the surrounding text is suspect too.
	{label: "markdown_beacon", re: regexp.MustCompile(`!?\[[^\]]*\]\((?:https?:)?//[^\s)]+\?[^\s)]+\)`), redact: false},
}

// Scanner is a configured outbound content scanner. The zero value is usable and
// matches only the built-in rules; use New to attach per-container literal needles.
type Scanner struct {
	literals []string // exact secret values that must never appear in output
}

// New builds a Scanner. literals are exact secret values (e.g. the real API token
// injected into a specific container's env on the raw-key path) that must never
// appear in any outbound message; an exact-substring hit is a hard block. Empty or
// whitespace-only literals are dropped so a misconfigured empty value can't match
// every message.
func New(literals ...string) *Scanner {
	var kept []string
	for _, l := range literals {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, l)
		}
	}
	return &Scanner{literals: kept}
}

// Scan satisfies delivery.OutboundScanner: it returns the (replacement, block,
// reason) tuple the delivery loop consumes, keeping that package free of any
// dependency on this one. It is a thin adapter over scan, which returns the richer
// Verdict used for testing and any future in-process callers.
func (s *Scanner) Scan(channel, chatID, text string) (replacement string, block bool, reason string) {
	v := s.scan(channel, chatID, text)
	return v.Replacement, v.Block, v.Reason
}

// scan inspects one outbound message body and returns a Verdict. channel and chatID
// are accepted for interface symmetry and future per-channel policy; the current
// rules are content-only. A Verdict with Block=true stops delivery; a Verdict with a
// non-empty Replacement and Block=false carries redacted text to send instead.
func (s *Scanner) scan(channel, chatID, text string) Verdict {
	if s == nil || text == "" {
		return Verdict{}
	}

	// 1. Exact literal needles first: the highest-confidence signal. Any hit is a
	//    hard block (a real injected secret has no business in a reply at all).
	for _, lit := range s.literals {
		if strings.Contains(text, lit) {
			return Verdict{Block: true, Reason: "outbound contains an injected container secret"}
		}
	}

	// 2. Pattern rules. Collect redactions; a single hard-block rule short-circuits.
	out := text
	redacted := false
	var redactedLabels []string
	for _, r := range rules {
		if !r.re.MatchString(out) {
			continue
		}
		if !r.redact {
			return Verdict{Block: true, Reason: "outbound matched " + r.label}
		}
		out = r.re.ReplaceAllString(out, redactMarker)
		redacted = true
		redactedLabels = append(redactedLabels, r.label)
	}
	if redacted {
		return Verdict{
			Block:       false,
			Reason:      "redacted " + strings.Join(redactedLabels, ","),
			Replacement: out,
		}
	}
	return Verdict{}
}

// String renders the scanner config for startup logging (counts only, never the
// literal values themselves).
func (s *Scanner) String() string {
	n := 0
	if s != nil {
		n = len(s.literals)
	}
	return fmt.Sprintf("outscan{rules:%d literals:%d}", len(rules), n)
}
