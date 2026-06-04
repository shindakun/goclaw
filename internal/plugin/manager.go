package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/shindakun/goclaw/internal/command"
)

// CommandSink is the slice of the command registry the manager needs. router's
// *command.Registry satisfies it. Kept narrow so this package does not import
// router.
type CommandSink interface {
	Register(command.Command)
	UnregisterSource(source string)
}

// Manager discovers plugins under a directory, launches the enabled ones, and
// registers their slash commands. It owns the live Clients and (later) supervises
// and hot-reloads them.
type Manager struct {
	dir      string      // the plugins/ root
	commands CommandSink // where plugin commands register
	hostEnv  []string    // base environment plugins inherit (host supplies secret values)
	log      *slog.Logger

	mu      sync.Mutex
	clients map[string]*Client // by plugin name
}

// NewManager creates a manager rooted at dir (the plugins/ directory). commands is
// where plugin slash commands are registered; hostEnv is the environment plugins
// inherit (so the host can pass credential values an entry's manifest declared by
// name).
func NewManager(dir string, commands CommandSink, hostEnv []string, log *slog.Logger) *Manager {
	return &Manager{
		dir:      dir,
		commands: commands,
		hostEnv:  hostEnv,
		log:      log,
		clients:  make(map[string]*Client),
	}
}

// LoadAll discovers every plugin directory under the root, launches the enabled
// ones, and registers their commands. It is the static (no-watch) startup path;
// the fsnotify reconciler comes later. A plugin that fails to load is logged and
// skipped, never fatal.
func (m *Manager) LoadAll(ctx context.Context, enabled func(name string) bool) error {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if os.IsNotExist(err) {
			m.log.Info("no plugins directory; none loaded", "dir", m.dir)
			return nil
		}
		return fmt.Errorf("plugin manager: read %q: %w", m.dir, err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(m.dir, e.Name())
		man, err := LoadManifest(dir)
		if err != nil {
			m.log.Warn("skipping plugin: bad manifest", "dir", dir, "err", err)
			continue
		}
		if enabled != nil && !enabled(man.Name) {
			m.log.Info("plugin present but disabled", "plugin", man.Name)
			continue
		}
		if err := m.launch(ctx, man); err != nil {
			m.log.Error("plugin launch failed", "plugin", man.Name, "err", err)
			continue
		}
	}
	return nil
}

// launch starts one plugin from its manifest, registers it, and wires its command.
func (m *Manager) launch(ctx context.Context, man Manifest) error {
	c, err := Launch(ctx, man.Name, man.ExecPath(), m.pluginEnv(man), m.log)
	if err != nil {
		return err
	}

	// Sanity: the handshake name should match the manifest name.
	if c.Info().Name != man.Name {
		_ = c.Close()
		return fmt.Errorf("plugin %q: handshake name %q disagrees with manifest", man.Name, c.Info().Name)
	}

	m.mu.Lock()
	if old, ok := m.clients[man.Name]; ok {
		// Replacing an existing plugin (hot reload): stop the old one first.
		_ = old.Close()
	}
	m.clients[man.Name] = c
	m.mu.Unlock()

	if man.Command != "" {
		m.registerCommand(man, c)
	}
	m.log.Info("plugin loaded", "plugin", man.Name, "kind", man.Kind, "version", man.Version,
		"tools", len(c.Info().Tools), "command", man.Command)
	return nil
}

// registerCommand wires a plugin's slash command to an Invoke of its primary tool.
func (m *Manager) registerCommand(man Manifest, c *Client) {
	// The command invokes the tool whose name matches the manifest command, or the
	// plugin's first advertised tool if there is no exact match.
	toolName := pickTool(man.Command, c.Info().Tools)

	m.commands.Register(command.Command{
		Name:        man.Command,
		Description: man.Description,
		Source:      man.Name,
		Handler: func(ctx context.Context, req command.Request) (string, error) {
			args, err := argsForTool(c.Info().Tools, toolName, req.Args)
			if err != nil {
				return "Usage: /" + man.Command + " <input>", nil
			}
			text, err := c.Invoke(ctx, toolName, args)
			if err != nil {
				return text, nil // a tool error: show the plugin's message, not a host error
			}
			return text, nil
		},
	})
}

// Close stops every plugin and removes its commands.
func (m *Manager) Close() {
	m.mu.Lock()
	clients := make([]*Client, 0, len(m.clients))
	for name, c := range m.clients {
		clients = append(clients, c)
		m.commands.UnregisterSource(name)
	}
	m.clients = make(map[string]*Client)
	m.mu.Unlock()

	for _, c := range clients {
		_ = c.Close()
	}
}

// Names returns the loaded plugin names, sorted.
func (m *Manager) Names() []string {
	m.mu.Lock()
	out := make([]string, 0, len(m.clients))
	for name := range m.clients {
		out = append(out, name)
	}
	m.mu.Unlock()
	sort.Strings(out)
	return out
}

// pluginEnv builds the environment for a plugin: the host base env (which carries
// any credential VALUES the host configured) plus nothing extra. The manifest's
// Env names which variables the plugin needs; for now we pass the whole host env,
// so a declared name is present if the host has it. A future tightening can filter
// to exactly the declared names.
func (m *Manager) pluginEnv(_ Manifest) []string {
	return m.hostEnv
}

// pickTool chooses which tool a command invokes: an exact name match, else the
// first advertised tool, else the command name itself (so a single-tool plugin
// whose tool is named like its command still works).
func pickTool(cmdName string, tools []ToolInfo) string {
	for _, t := range tools {
		if t.Name == cmdName {
			return t.Name
		}
	}
	if len(tools) > 0 {
		return tools[0].Name
	}
	return cmdName
}

// argsForTool maps a slash command's raw argument STRING to the tool's JSON args.
// For a tool whose input schema is a single string property, the raw string fills
// that property. A tool with no args ignores the string. Anything more complex is
// not auto-mappable from a single line and returns an error so the handler can show
// usage.
func argsForTool(tools []ToolInfo, toolName, raw string) (json.RawMessage, error) {
	var schema []byte
	for _, t := range tools {
		if t.Name == toolName {
			schema = t.InputSchema
			break
		}
	}
	field, ok := singleStringProperty(schema)
	if !ok {
		// No schema, or not a single-string shape: pass an empty object and let the
		// tool validate. (A no-arg tool is fine; a multi-field tool will report its
		// own error.)
		if raw == "" {
			return json.RawMessage(`{}`), nil
		}
		return nil, fmt.Errorf("tool %q does not take a single string argument", toolName)
	}
	obj := map[string]string{field: raw}
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// singleStringProperty reports the property name when a JSON Schema object has
// exactly one property and it is a string. Returns ("", false) otherwise.
func singleStringProperty(schema json.RawMessage) (string, bool) {
	if len(schema) == 0 {
		return "", false
	}
	var s struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return "", false
	}
	if s.Type != "object" || len(s.Properties) != 1 {
		return "", false
	}
	for name, p := range s.Properties {
		if p.Type == "string" {
			return name, true
		}
	}
	return "", false
}
