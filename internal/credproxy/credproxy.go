// Package credproxy is the bundled credential proxy (brief §8). The host runs a
// TLS-intercepting proxy (see mitmproxy.go); runner containers route their HTTPS
// through it via HTTPS_PROXY and trust its CA. For each request to a host with a
// stored credential, the proxy injects the real token (api.anthropic.com ->
// x-api-key, otherwise Authorization: Bearer) and forwards to the real API, so
// the raw token lives only in host memory and never enters the container. Hosts
// with no stored credential are blind-tunneled (never decrypted).
package credproxy

import (
	"net/http"
	"strings"
)

// injectAuth removes any inbound auth headers and sets the correct one for the
// upstream host: api.anthropic.com uses x-api-key (raw value); everything else
// uses Authorization: Bearer (the inferred-from-host rule). The host argument is
// the LOGICAL host the credential is for, not a rewritten upstream address.
func injectAuth(req *http.Request, host, token string) {
	req.Header.Del("Authorization")
	req.Header.Del("x-api-key")
	req.Header.Del("X-Api-Key")
	if isAnthropic(host) {
		req.Header.Set("x-api-key", token)
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func isAnthropic(host string) bool {
	return host == "api.anthropic.com" || strings.HasSuffix(host, ".anthropic.com")
}
