package plugin

import (
	"strings"
	"testing"
)

func TestValidateSubdir(t *testing.T) {
	ok := []string{
		"",          // empty = repo root, allowed
		"cmd/gmail", // the monorepo case
		"cmd/gmail-tools",
		"plugins/roll",
		"a",
	}
	for _, s := range ok {
		if err := validateSubdir(s); err != nil {
			t.Errorf("validateSubdir(%q) = %v, want nil", s, err)
		}
	}

	bad := map[string]string{
		"absolute":         "/etc",
		"leading slash":    "/cmd/gmail",
		"trailing slash":   "cmd/gmail/",
		"parent traversal": "cmd/../../etc",
		"dotdot only":      "..",
		"dot segment":      "cmd/./gmail",
		"empty segment":    "cmd//gmail",
		"space":            "cmd/gma il",
		"semicolon inject": "cmd/gmail;rm -rf /",
		"dollar inject":    "cmd/$HOME",
		"backtick inject":  "cmd/`id`",
		"glob star":        "cmd/*",
		"tilde":            "~/cmd",
		"backslash":        "cmd\\gmail",
	}
	for name, s := range bad {
		if err := validateSubdir(s); err == nil {
			t.Errorf("%s: validateSubdir(%q) = nil, want rejection", name, s)
		}
	}
}

// The spec "<url>#<subdir>" splits as expected, and a plain URL has no subdir. (This
// mirrors what Add does before validating each part.)
func TestSpecSplit(t *testing.T) {
	cases := []struct{ spec, url, sub string }{
		{"https://github.com/x/roll", "https://github.com/x/roll", ""},
		{"https://github.com/x/goclaw-gmail#cmd/gmail", "https://github.com/x/goclaw-gmail", "cmd/gmail"},
		{"https://github.com/x/goclaw-gmail#cmd/gmail-tools", "https://github.com/x/goclaw-gmail", "cmd/gmail-tools"},
	}
	for _, c := range cases {
		url, sub, _ := strings.Cut(c.spec, "#")
		if url != c.url || sub != c.sub {
			t.Errorf("split %q = (%q, %q), want (%q, %q)", c.spec, url, sub, c.url, c.sub)
		}
		// Both parts must validate for a well-formed monorepo spec.
		if err := validateGitURL(url); err != nil {
			t.Errorf("validateGitURL(%q): %v", url, err)
		}
		if err := validateSubdir(sub); err != nil {
			t.Errorf("validateSubdir(%q): %v", sub, err)
		}
	}
}
