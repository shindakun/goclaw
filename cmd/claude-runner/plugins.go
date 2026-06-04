package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	claude "github.com/shindakun/agent-sdk-go"
	"github.com/shindakun/goclaw/internal/plugin"
)

// pluginsDir is where the host mounts installed plugins, read-only. The runner
// discovers and launches them HERE, inside the container, so untrusted plugin code
// runs in the agent's sandbox and never on the host.
const pluginsDir = "/plugins"

// pluginHost owns the plugin clients launched inside the container and the SDK MCP
// server that exposes their tools to the agent. A plugin tool becomes a local MCP
// tool the agent can call in-process; there is no host round-trip.
type pluginHost struct {
	clients []*plugin.Client
	server  *claude.SdkMcpServer
	// cmds maps a slash-command word (no slash) to the plugin client and tool it
	// invokes, so a user's "/roll 2d6" dispatches directly without an LLM turn.
	cmds map[string]boundCommand
	log  *slog.Logger
}

// boundCommand ties a declared slash command to the client and tool it invokes.
type boundCommand struct {
	client *plugin.Client
	tool   plugin.ToolInfo
}

// loadPlugins discovers plugin dirs under /plugins, launches each enabled plugin as
// a child process inside the container, and builds an SDK MCP server exposing every
// advertised tool. A plugin that fails to load is logged and skipped, never fatal.
// Returns nil (no server) when there are no plugins, so the caller adds no MCP opt.
func loadPlugins(ctx context.Context, dir string, log *slog.Logger) *pluginHost {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn("plugins: read dir", "dir", dir, "err", err)
		}
		return nil
	}

	ph := &pluginHost{log: log, cmds: map[string]boundCommand{}}
	server := claude.NewSdkMcpServer("goclaw-plugins")
	added := 0

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pdir := filepath.Join(dir, e.Name())
		man, err := plugin.LoadManifest(pdir)
		if err != nil {
			log.Warn("plugins: skip bad manifest", "dir", pdir, "err", err)
			continue
		}
		c, err := plugin.Launch(ctx, man.Name, man.ExecPath(), os.Environ(), log)
		if err != nil {
			log.Error("plugins: launch failed", "plugin", man.Name, "err", err)
			continue
		}
		ph.clients = append(ph.clients, c)
		for _, t := range c.Info().Tools {
			server.AddTool(pluginTool(c, t))
			added++
		}
		// Bind the plugin's declared slash command (if any) to a tool: an exact
		// name match, else the first advertised tool.
		if man.Command != "" {
			if t, ok := commandTool(man.Command, c.Info().Tools); ok {
				ph.cmds[man.Command] = boundCommand{client: c, tool: t}
			}
		}
		log.Info("plugin loaded", "plugin", man.Name, "tools", len(c.Info().Tools), "command", man.Command)
	}

	if added == 0 {
		ph.Close()
		return nil
	}
	ph.server = server
	return ph
}

// pluginTool builds an SDK MCP tool that forwards an agent tool call to the plugin
// over the frame protocol, preserving the plugin's own input schema.
func pluginTool(c *plugin.Client, t plugin.ToolInfo) claude.Tool {
	return claude.Tool{
		Name:        t.Name,
		Description: t.Description,
		InputSchema: t.InputSchema,
		Handler: func(ctx context.Context, args json.RawMessage) (claude.ToolResult, error) {
			text, err := c.Invoke(ctx, t.Name, args)
			if err != nil {
				// A plugin/tool error becomes an error result the agent sees, not a
				// runner failure.
				return claude.ErrorResult(text), nil
			}
			return claude.TextResult(text), nil
		},
	}
}

// command dispatches a user slash command to a matching plugin tool, returning the
// reply and true when it handled it. It returns ("", false) when the text is not a
// slash command or no plugin owns it, so the caller falls through to the agent.
func (ph *pluginHost) command(ctx context.Context, text string) (string, bool) {
	if ph == nil || len(ph.cmds) == 0 {
		return "", false
	}
	name, args, ok := parseSlash(text)
	if !ok {
		return "", false
	}
	bc, ok := ph.cmds[name]
	if !ok {
		return "", false
	}
	in, err := argsForTool(bc.tool, args)
	if err != nil {
		return "Usage: /" + name + " <input>", true
	}
	text, err = bc.client.Invoke(ctx, bc.tool.Name, in)
	if err != nil {
		// Show the plugin's error message to the user rather than a runner error.
		return text, true
	}
	return text, true
}

// parseSlash splits "/word args" into (word, args, true); word is lower-cased and
// the args are trimmed. Non-slash text yields ok=false.
func parseSlash(text string) (name, args string, ok bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "/") || len(t) == 1 {
		return "", "", false
	}
	word, rest, _ := strings.Cut(t[1:], " ")
	if word == "" {
		return "", "", false
	}
	return strings.ToLower(word), strings.TrimSpace(rest), true
}

// commandTool picks the tool a command invokes: an exact name match, else the first
// advertised tool.
func commandTool(cmd string, tools []plugin.ToolInfo) (plugin.ToolInfo, bool) {
	for _, t := range tools {
		if t.Name == cmd {
			return t, true
		}
	}
	if len(tools) > 0 {
		return tools[0], true
	}
	return plugin.ToolInfo{}, false
}

// argsForTool maps a slash command's raw argument STRING to the tool's JSON args.
// For a tool whose input is a single string property, the raw string fills it. A
// no-arg call yields an empty object. A multi-field tool cannot be filled from one
// line and returns an error so the caller shows usage.
func argsForTool(t plugin.ToolInfo, raw string) (json.RawMessage, error) {
	field, ok := singleStringProperty(t.InputSchema)
	if !ok {
		if raw == "" {
			return json.RawMessage(`{}`), nil
		}
		return nil, errMultiField
	}
	b, err := json.Marshal(map[string]string{field: raw})
	if err != nil {
		return nil, err
	}
	return b, nil
}

var errMultiField = errors.New("tool does not take a single string argument")

// singleStringProperty returns the property name when a JSON Schema object has
// exactly one property and it is a string; ("", false) otherwise.
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

// Close stops every launched plugin.
func (ph *pluginHost) Close() {
	if ph == nil {
		return
	}
	for _, c := range ph.clients {
		_ = c.Close()
	}
	ph.clients = nil
}
