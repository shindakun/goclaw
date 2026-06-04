// Package credproxy is the bundled credential proxy (brief §8). The host runs a
// TLS-intercepting proxy (see mitmproxy.go); runner containers route their HTTPS
// through it via HTTPS_PROXY and trust its CA. For each request to a host with a
// stored credential, the proxy injects the real token (api.anthropic.com ->
// x-api-key, otherwise Authorization: Bearer) and forwards to the real API, so
// the raw token lives only in host memory and never enters the container. Hosts
// with no stored credential are blind-tunneled (never decrypted).
package credproxy

import (
	"encoding/base64"
	"net/http"
	"strings"
)

// injectAuth removes any inbound auth headers and sets the correct one for the
// upstream host (the LOGICAL host the credential is for, not a rewritten upstream
// address). The scheme is host-specific because the services differ:
//   - Anthropic (api.anthropic.com): x-api-key: <token>
//   - GitHub git smart-HTTP (github.com, codeload.github.com): HTTP Basic with
//     x-access-token:<token> - the git endpoints REJECT Bearer (verified) and
//     answer with WWW-Authenticate: Basic.
//   - everything else, incl. the GitHub API (api.github.com): Authorization:
//     Bearer <token>.
func injectAuth(req *http.Request, host, token string) {
	req.Header.Del("Authorization")
	req.Header.Del("x-api-key")
	req.Header.Del("X-Api-Key")
	switch {
	case isAnthropic(host):
		req.Header.Set("x-api-key", token)
	case isGitHubGit(host):
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		req.Header.Set("Authorization", "Basic "+basic)
	default:
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func isAnthropic(host string) bool {
	return host == "api.anthropic.com" || strings.HasSuffix(host, ".anthropic.com")
}

// isGitHubGit reports whether host is a GitHub git smart-HTTP endpoint, which
// authenticates with HTTP Basic (x-access-token:<token>) rather than Bearer. The
// API host api.github.com is deliberately excluded (it uses Bearer).
func isGitHubGit(host string) bool {
	return host == "github.com" || host == "codeload.github.com"
}
