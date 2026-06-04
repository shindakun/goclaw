package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	claude "github.com/shindakun/agent-sdk-go"
	"github.com/shindakun/goclaw/internal/plugin"
)

// pluginsDir is where the host mounts installed plugins, read-only. The runner
// discovers and launches them HERE, inside the container, so untrusted plugin code
// runs in the agent's sandbox and never on the host. The host stages a newly
// installed plugin into this dir; an fsnotify watch reloads it live (no container
// restart).
const pluginsDir = "/plugins"

// pluginHost owns the plugin clients launched inside the container, the SDK MCP
// server that exposes their tools to the agent, and a watcher that hot-loads
// plugins as the host installs or removes them. A plugin tool becomes a local MCP
// tool the agent can call in-process; there is no host round-trip.
type pluginHost struct {
	server *claude.SdkMcpServer
	log    *slog.Logger
	dir    string

	mu      sync.Mutex
	loaded  map[string]*loadedPlugin // by plugin name
	cmds    map[string]boundCommand  // slash command (no slash) -> client+tool
	watcher *fsnotify.Watcher
}

// loadedPlugin is one running plugin and the dir it was loaded from.
type loadedPlugin struct {
	client *plugin.Client
	dir    string
}

// boundCommand ties a declared slash command to the client and tool it invokes.
type boundCommand struct {
	client *plugin.Client
	tool   plugin.ToolInfo
}

// startPlugins creates the plugin host, loads whatever is already installed, and
// starts watching the plugins dir for live add/remove. It always returns a non-nil
// host (so newly installed plugins can appear later) with a non-nil MCP server.
func startPlugins(ctx context.Context, dir string, log *slog.Logger) *pluginHost {
	ph := &pluginHost{
		server: claude.NewSdkMcpServer("goclaw-plugins"),
		log:    log,
		dir:    dir,
		loaded: map[string]*loadedPlugin{},
		cmds:   map[string]boundCommand{},
	}
	ph.reconcile(ctx) // load what is already there
	ph.startWatch(ctx)
	return ph
}

// reconcile diffs the plugins dir against the loaded set: launch newly present
// plugins, stop ones whose dir is gone. Safe to call repeatedly (on a watch event).
func (ph *pluginHost) reconcile(ctx context.Context) {
	entries, err := os.ReadDir(ph.dir)
	if err != nil {
		if !os.IsNotExist(err) {
			ph.log.Warn("plugins: read dir", "dir", ph.dir, "err", err)
		}
		// Dir absent: drop everything loaded.
		ph.dropMissing(map[string]bool{})
		return
	}

	present := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue // skip non-dirs and hidden dirs (e.g. an in-progress .build-*)
		}
		pdir := filepath.Join(ph.dir, e.Name())
		man, err := plugin.LoadManifest(pdir)
		if err != nil {
			continue // half-installed or malformed; skip until complete
		}
		present[man.Name] = true

		ph.mu.Lock()
		_, already := ph.loaded[man.Name]
		ph.mu.Unlock()
		if already {
			continue
		}
		ph.launch(ctx, man, pdir)
	}
	ph.dropMissing(present)
}

// launch starts one plugin, registers its tools on the MCP server, and binds its
// slash command. Errors are logged, never fatal.
func (ph *pluginHost) launch(ctx context.Context, man plugin.Manifest, pdir string) {
	c, err := plugin.Launch(ctx, man.Name, man.ExecPath(), os.Environ(), ph.log)
	if err != nil {
		ph.log.Error("plugins: launch failed", "plugin", man.Name, "err", err)
		return
	}
	ph.mu.Lock()
	ph.loaded[man.Name] = &loadedPlugin{client: c, dir: pdir}
	for _, t := range c.Info().Tools {
		ph.server.AddTool(pluginTool(c, t))
	}
	if man.Command != "" {
		if t, ok := commandTool(man.Command, c.Info().Tools); ok {
			ph.cmds[man.Command] = boundCommand{client: c, tool: t}
		}
	}
	ph.mu.Unlock()
	ph.log.Info("plugin loaded", "plugin", man.Name, "tools", len(c.Info().Tools), "command", man.Command)
}

// dropMissing stops and removes any loaded plugin whose name is not in present.
func (ph *pluginHost) dropMissing(present map[string]bool) {
	ph.mu.Lock()
	var stop []*loadedPlugin
	for name, lp := range ph.loaded {
		if present[name] {
			continue
		}
		stop = append(stop, lp)
		delete(ph.loaded, name)
		// Drop any slash command bound to this client.
		for word, bc := range ph.cmds {
			if bc.client == lp.client {
				delete(ph.cmds, word)
			}
		}
	}
	ph.mu.Unlock()
	for _, lp := range stop {
		ph.log.Info("plugin unloaded", "dir", lp.dir)
		_ = lp.client.Close()
	}
	// Note: the MCP server keeps a now-dead tool registered until the next query
	// rebuilds the session; an invoke of it returns an error result (the client is
	// closed), which is acceptable and self-corrects on the next turn.
}

// startWatch begins an fsnotify watch on the plugins dir; on any change it
// reconciles. The watch stops when ctx is cancelled.
func (ph *pluginHost) startWatch(ctx context.Context) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		ph.log.Warn("plugins: watcher unavailable; no hot reload", "err", err)
		return
	}
	// Watch the parent dir so we see subdir create/remove (a plugin is a subdir).
	if err := os.MkdirAll(ph.dir, 0o755); err == nil {
		if err := w.Add(ph.dir); err != nil {
			ph.log.Warn("plugins: watch add failed; no hot reload", "err", err)
			_ = w.Close()
			return
		}
	}
	ph.watcher = w
	go func() {
		// Debounce: an install writes several files (binary, plugin.yml) and a
		// rename; coalesce a burst into one reconcile shortly after the last event.
		const debounce = 400 * time.Millisecond
		var timer *time.Timer
		var timerC <-chan time.Time
		for {
			select {
			case <-ctx.Done():
				_ = w.Close()
				return
			case _, ok := <-w.Events:
				if !ok {
					return
				}
				if timer == nil {
					timer = time.NewTimer(debounce)
				} else {
					timer.Reset(debounce)
				}
				timerC = timer.C // re-arm: select this timer until it fires
			case <-timerC:
				timerC = nil // disarm until the next event re-arms it
				ph.reconcile(ctx)
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				ph.log.Warn("plugins: watch error", "err", err)
			}
		}
	}()
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
				return claude.ErrorResult(text), nil
			}
			return claude.TextResult(text), nil
		},
	}
}

// command dispatches a user slash command to a matching plugin tool, returning the
// reply and true when it handled it. Returns ("", false) when not a plugin command.
func (ph *pluginHost) command(ctx context.Context, text string) (string, bool) {
	if ph == nil {
		return "", false
	}
	name, args, ok := parseSlash(text)
	if !ok {
		return "", false
	}
	ph.mu.Lock()
	bc, ok := ph.cmds[name]
	ph.mu.Unlock()
	if !ok {
		return "", false
	}
	in, err := argsForTool(bc.tool, args)
	if err != nil {
		return "Usage: /" + name + " <input>", true
	}
	text, err = bc.client.Invoke(ctx, bc.tool.Name, in)
	if err != nil {
		return text, true // show the plugin's error message
	}
	return text, true
}

// hasServerTools reports whether any plugin tool is registered (so the caller adds
// the MCP option only when there is something to expose).
func (ph *pluginHost) hasServerTools() bool {
	if ph == nil {
		return false
	}
	ph.mu.Lock()
	defer ph.mu.Unlock()
	return len(ph.loaded) > 0
}

// parseSlash splits "/word args" into (word, args, true); word is lower-cased and
// args trimmed. Non-slash text yields ok=false.
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
// A single-string-property schema is filled from the raw string; a no-arg call
// yields an empty object; anything else returns an error so the caller shows usage.
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

// Close stops every launched plugin and the watcher.
func (ph *pluginHost) Close() {
	if ph == nil {
		return
	}
	if ph.watcher != nil {
		_ = ph.watcher.Close()
	}
	ph.mu.Lock()
	clients := make([]*plugin.Client, 0, len(ph.loaded))
	for _, lp := range ph.loaded {
		clients = append(clients, lp.client)
	}
	ph.loaded = map[string]*loadedPlugin{}
	ph.cmds = map[string]boundCommand{}
	ph.mu.Unlock()
	for _, c := range clients {
		_ = c.Close()
	}
}
