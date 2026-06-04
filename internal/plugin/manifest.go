package plugin

import (
	"fmt"
	"os"
	"path/filepath"

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
	switch m.Kind {
	case "tool":
		// ok
	case "channel":
		return fmt.Errorf("kind %q not supported yet", m.Kind)
	default:
		return fmt.Errorf("unknown kind %q", m.Kind)
	}
	return nil
}
