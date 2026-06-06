package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Manifest is a plugin's at-rest, pre-launch self-description: the plugin.yml the
// author ships in the plugin's directory. The host reads it BEFORE launching to
// learn the plugin's identity, the binary to run, any env var names it needs, and
// the slash command it registers. The runtime hello handshake stays the source of
// truth for the live tool list; this is what the host knows before the process
// starts.
type Manifest struct {
	Name        string   `yaml:"name"`        // stable id; must match handshake Info.Name
	Kind        string   `yaml:"kind"`        // "tool" now ("channel" later)
	Version     string   `yaml:"version"`     // the plugin's own version
	Author      string   `yaml:"author"`      // free-form; shown in plugin listings
	URL         string   `yaml:"url"`         // source/home (git or web)
	Exec        string   `yaml:"exec"`        // binary, relative to the plugin dir
	Description string   `yaml:"description"` // shown in /commands when it has a command
	Command     string   `yaml:"command"`     // slash command to register; "" = none
	Env         []string `yaml:"env"`         // env var NAMES the plugin needs (values from host)

	// dir is the plugin's directory, filled by LoadManifest (not from the file).
	dir string
}

// ExecPath returns the absolute path to the plugin binary (Exec resolved against
// the plugin's directory).
func (m Manifest) ExecPath() string {
	if filepath.IsAbs(m.Exec) {
		return m.Exec
	}
	return filepath.Join(m.dir, m.Exec)
}

// proxyRoutingEnv are the env vars a plugin needs to reach external APIs THROUGH the
// credential proxy: the proxy address and the proxy-CA path. They are NOT secrets (the
// proxy is a localhost address; the CA is a public cert), so passing them does not violate
// the "no host secrets to plugins" rule. They are in fact REQUIRED for the proxy design to
// work: a target-mode plugin (e.g. gmail) holds no token and relies on the proxy injecting
// one, but Go's http.ProxyFromEnvironment and TLS trust only consult these vars. Without
// them the plugin connects DIRECTLY to the upstream, sends no auth, and gets a 401 (the
// proxy never sees the request). Both letter cases are included because Go reads the
// lowercase forms for proxy selection and some libraries read the uppercase.
var proxyRoutingEnv = []string{
	"HTTPS_PROXY", "https_proxy",
	"HTTP_PROXY", "http_proxy",
	"NO_PROXY", "no_proxy",
	"SSL_CERT_FILE", // the proxy CA, so the plugin trusts the proxy's intercept leaf
}

// MinimalEnvBase is the secret-free base environment a plugin process starts from,
// before the manifest's allowlisted names are added (see InjectEnv). It carries PATH (a
// plugin binary may need it to resolve shared libs or sub-tools) plus the proxy-routing
// vars (so the plugin can reach the credential proxy; see proxyRoutingEnv). Nothing else
// from the host environment, and never a secret, should reach a plugin. Deliberately NOT
// os.Environ().
func MinimalEnvBase() []string {
	var base []string
	if p, ok := os.LookupEnv("PATH"); ok {
		base = append(base, "PATH="+p)
	}
	for _, name := range proxyRoutingEnv {
		if v, ok := os.LookupEnv(name); ok {
			base = append(base, name+"="+v)
		}
	}
	return base
}

// InjectEnv builds the environment for a plugin process as base PLUS only the
// manifest's declared env names (resolved via lookup), and NOTHING else. The env list
// is an ALLOWLIST: the plugin declares the names it needs, and only those cross. base
// must be a MINIMAL, host-curated environment (e.g. just PATH), never os.Environ():
// the host environment holds secrets (TELEGRAM_BOT_TOKEN, GOCLAW_ANTHROPIC_API_KEY,
// PATs) that must never reach a plugin, least of all an untrusted one running in (or
// destined for) the container. Leaking the full env would defeat the credential proxy,
// whose entire purpose is keeping real tokens out of the container.
//
// lookup supplies the value for each declared name (typically os.LookupEnv, which
// after config.Load also carries .env). A name not found is SKIPPED, so the plugin
// falls back to its own default rather than getting an empty override. A later config
// source (per-plugin settings) can replace lookup without changing callers.
func (m Manifest) InjectEnv(base []string, lookup func(string) (string, bool)) []string {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	out := make([]string, len(base), len(base)+len(m.Env))
	copy(out, base)
	for _, name := range m.Env {
		if v, ok := lookup(name); ok {
			out = append(out, name+"="+v)
		}
	}
	return out
}

// Dir returns the plugin's directory.
func (m Manifest) Dir() string { return m.dir }

// LoadManifest reads and validates a plugin.yml from a plugin directory. It does
// NOT check that the binary exists or is runnable; that surfaces at launch.
func LoadManifest(dir string) (Manifest, error) {
	path := filepath.Join(dir, "plugin.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("plugin manifest %q: %w", path, err)
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("plugin manifest %q: %w", path, err)
	}
	m.dir = dir
	if err := m.validate(); err != nil {
		return Manifest{}, fmt.Errorf("plugin manifest %q: %w", path, err)
	}
	return m, nil
}

// validate checks the required fields are present and the kind is known.
func (m Manifest) validate() error {
	if m.Name == "" {
		return fmt.Errorf("missing name")
	}
	if m.Exec == "" {
		return fmt.Errorf("missing exec")
	}
	// exec is a bare binary filename relative to the plugin dir. Reject a path,
	// whitespace, or anything that could escape the dir or make the host's Go
	// parser and the build script's shell parser disagree on the name.
	if m.Exec != filepath.Base(m.Exec) || strings.ContainsAny(m.Exec, " \t/\\") {
		return fmt.Errorf("exec %q must be a bare filename (no path or spaces)", m.Exec)
	}
	switch m.Kind {
	case "tool", "channel":
		// ok. A channel manifest has no command (a channel is not a slash command);
		// its env: list names the config it needs (host supplies values via InjectEnv).
	default:
		return fmt.Errorf("unknown kind %q", m.Kind)
	}
	if m.Kind == "channel" && m.Command != "" {
		return fmt.Errorf("channel %q must not declare a command (a channel is not a slash command)", m.Name)
	}
	return nil
}
