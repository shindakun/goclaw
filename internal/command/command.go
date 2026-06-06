// Package command is the host's slash-command registry. Built-in commands
// (/commands, /approve, ...) and plugin-provided commands (e.g. /roll from the
// roll plugin) all register here, so there is one place that knows every command,
// one dispatch path, and one source for the /commands listing.
//
// The registry is concurrency-safe: plugin commands are added and removed at
// runtime as plugins are enabled, disabled, or hot-reloaded.
package command

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/shindakun/goclaw/internal/permissions"
)

// Request is the resolved context a command handler needs. It is channel-agnostic:
// the router fills it from an inbound message plus the resolved user.
type Request struct {
	Channel  string
	ChatID   string
	SenderID string
	Sender   string // display name, best-effort
	UserID   int64  // resolved user id (0 if unknown); for owner-scoped commands
	Role     permissions.Role
	IsKnown  bool   // whether the sender resolved to a known user
	Args     string // the command line after the command word, trimmed
}

// Handler runs a command and returns the text to send back to the caller. An empty
// string means "no reply" (the handler delivered its own output out of band).
type Handler func(ctx context.Context, req Request) (reply string, err error)

// Command is one registered slash command.
type Command struct {
	// Name is the command word WITHOUT the leading slash ("commands", "roll").
	Name string
	// Description is the one-line summary shown by /commands.
	Description string
	// MinRole gates who may run (and see) the command. RoleMember is the lowest
	// real role; use it for commands any known user may run. An empty MinRole is
	// treated as RoleMember.
	MinRole permissions.Role
	// Source labels where the command came from ("builtin", or a plugin name), for
	// grouping in /commands and for bulk-removal when a plugin is unloaded.
	Source string
	// PassThrough marks a command the HOST does not execute: it is registered only
	// so /commands lists it, and the router lets the message flow on to its real
	// handler. /reset and /compact are pass-through because the agent runner (inside
	// the container) acts on the session, not the host. A pass-through command has
	// no Handler.
	PassThrough bool
	// Handler runs the command (nil for a PassThrough command).
	Handler Handler
}

// Registry holds the registered commands, keyed by name.
type Registry struct {
	mu  sync.RWMutex
	cmd map[string]Command
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{cmd: make(map[string]Command)}
}

// Register adds or replaces a command. The name is normalized (a leading slash is
// stripped, lower-cased). Replacing is allowed so a hot-reloaded plugin can update
// its command in place.
func (r *Registry) Register(c Command) {
	c.Name = normalize(c.Name)
	if c.MinRole == "" {
		c.MinRole = permissions.RoleMember
	}
	r.mu.Lock()
	r.cmd[c.Name] = c
	r.mu.Unlock()
}

// Unregister removes a command by name.
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	delete(r.cmd, normalize(name))
	r.mu.Unlock()
}

// UnregisterSource removes every command from a given source (e.g. all commands a
// plugin registered, when it is disabled).
func (r *Registry) UnregisterSource(source string) {
	r.mu.Lock()
	for name, c := range r.cmd {
		if c.Source == source {
			delete(r.cmd, name)
		}
	}
	r.mu.Unlock()
}

// Get returns the command for a name (slash optional), or false if absent.
func (r *Registry) Get(name string) (Command, bool) {
	r.mu.RLock()
	c, ok := r.cmd[normalize(name)]
	r.mu.RUnlock()
	return c, ok
}

// Visible returns the commands a caller with the given role may run, sorted by
// name. A command is visible when the caller's role is at least its MinRole.
func (r *Registry) Visible(role permissions.Role) []Command {
	r.mu.RLock()
	out := make([]Command, 0, len(r.cmd))
	for _, c := range r.cmd {
		if roleAtLeast(role, c.MinRole) {
			out = append(out, c)
		}
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// IsCommand reports whether text begins with a slash command word, returning the
// normalized name and the remaining argument string. It does not check the registry.
func IsCommand(text string) (name, args string, ok bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "/") || len(t) == 1 {
		return "", "", false
	}
	word, rest, _ := strings.Cut(t[1:], " ")
	if word == "" {
		return "", "", false
	}
	return normalize(word), strings.TrimSpace(rest), true
}

func normalize(name string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "/"))
}

// roleAtLeast reports whether have meets or exceeds need in the owner > admin >
// member ordering. An unknown/empty role is treated as below member.
func roleAtLeast(have, need permissions.Role) bool {
	return rank(have) >= rank(need)
}

func rank(role permissions.Role) int {
	switch role {
	case permissions.RoleOwner:
		return 3
	case permissions.RoleAdmin:
		return 2
	case permissions.RoleMember:
		return 1
	default:
		return 0
	}
}
