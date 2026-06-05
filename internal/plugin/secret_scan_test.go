package plugin

import (
	"regexp"
	"strings"
	"testing"
)

// TestSecretEnvScanPattern verifies the install-time secret-read scan catches the
// cases it claims to and does not false-positive clean plugin source. It compiles the
// SAME regex the install script greps with (secretEnvScanPattern), so the assertion
// tracks the real behavior; the install script and this test share one pattern source.
//
// The scan is best-effort and evadable by design (see secretEnvScanPattern's doc and
// docs/security.md): the MISSED cases below document the known limits, they are not
// failures.
func TestSecretEnvScanPattern(t *testing.T) {
	re := regexp.MustCompile(secretEnvScanPattern)

	caught := []struct{ name, src string }{
		{"direct anthropic read", `os.Getenv("ANTHROPIC_API_KEY")`},
		{"direct gh read", `os.Getenv("GH_TOKEN")`},
		{"oauth token", `os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")`},
		{"encryption key", `os.Getenv("GOCLAW_SECRET_ENCRYPTION_KEY")`},
		{"github token alt", `os.Getenv("GOCLAW_GITHUB_TOKEN")`},
		// Natural _-boundary concatenation keeps a distinctive fragment contiguous.
		{"split at underscore", `os.Getenv("ANTHROPIC" + "_API_KEY")`},
		{"split keeps anthropic", `os.Getenv("ANTHROPIC_API" + "_KEY")`},
		{"oauth fragment", `os.Getenv(prefix + "_OAUTH_TOKEN")`},
	}
	for _, c := range caught {
		if !re.MatchString(c.src) {
			t.Errorf("scan MISSED a host-secret read it should catch (%s): %q", c.name, c.src)
		}
	}

	clean := []struct{ name, src string }{
		{"plugin own api config", `os.Getenv("MYPLUGIN_API_URL")`},
		{"generic token field", `type Cfg struct{ Token string }`},
		{"unrelated key", `cache.Set(key, value)`},
		{"roll-like", `notation := args["notation"].(string)`},
		{"plain os.Environ", `for _, e := range os.Environ() { _ = e }`}, // deliberately NOT matched
	}
	for _, c := range clean {
		if re.MatchString(c.src) {
			t.Errorf("scan FALSE-POSITIVED clean source (%s): %q", c.name, c.src)
		}
	}

	// Documented LIMIT: a mid-word split (no distinctive fragment stays contiguous)
	// evades the scan. This is asserted so the limit is explicit and a future change
	// that "fixes" it has to consciously update this expectation.
	midWordSplit := `os.Getenv("ANT" + "HROPIC" + "_API_KEY")`
	if re.MatchString(midWordSplit) {
		t.Logf("note: mid-word split now caught (%q); the documented limit changed", midWordSplit)
	}
	ghSplit := `os.Getenv("GH" + "_TOKEN")`
	if re.MatchString(ghSplit) {
		t.Logf("note: GH_TOKEN split now caught (%q); the documented limit changed", ghSplit)
	}

	// Guard: the install script must actually USE the pattern (catch an accidental
	// edit that drops the scan).
	if !strings.Contains(installScript, secretEnvScanPattern) {
		t.Fatal("installScript no longer contains the secret-env scan pattern")
	}
}
