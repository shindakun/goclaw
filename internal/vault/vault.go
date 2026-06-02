// Package vault wires the OneCLI Agent Vault credential proxy at container
// spawn time (brief §8). The vault injects credentials at request time so raw
// API keys never enter the container; the host only sets proxy env vars and
// mounts the vault CA. This is language-agnostic env + mounts.
package vault

import "github.com/shindakun/goclaw/internal/mounts"

// Config describes how to point a container at the OneCLI vault.
type Config struct {
	// ProxyURL is the vault's HTTP(S) proxy endpoint.
	ProxyURL string
	// NoProxy is the comma-separated NO_PROXY value.
	NoProxy string
	// CAHostPath is the host path to the vault's CA certificate.
	CAHostPath string
	// CAContainerPath is where the CA is mounted in the container trust store.
	CAContainerPath string
	// AgentID is the per-agent-group OneCLI identifier (per-group scoping).
	AgentID string
}

// Reachable reports whether the vault endpoint is currently reachable.
//
// TODO: implement a real health probe. On unreachable, the caller starts the
// container with NO credentials and logs a warning (brief §8 — match current
// behavior, fail open to a credential-less container rather than blocking).
func (c Config) Reachable() bool { return c.ProxyURL != "" }

// Env returns the proxy environment variables to set on the container.
func (c Config) Env() map[string]string {
	if c.ProxyURL == "" {
		return nil
	}
	return map[string]string{
		"HTTP_PROXY":  c.ProxyURL,
		"HTTPS_PROXY": c.ProxyURL,
		"NO_PROXY":    c.NoProxy,
	}
}

// CAMount returns the read-only CA mount, if a CA path is configured.
func (c Config) CAMount() (mounts.Mount, bool) {
	if c.CAHostPath == "" || c.CAContainerPath == "" {
		return mounts.Mount{}, false
	}
	return mounts.Mount{
		HostPath:      c.CAHostPath,
		ContainerPath: c.CAContainerPath,
		ReadWrite:     false,
	}, true
}
