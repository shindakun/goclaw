package command

import (
	"context"
	"fmt"
	"strings"

	"github.com/shindakun/goclaw/internal/permissions"
)

// RegisterListing adds the built-in "/commands" listing command (and a "/help"
// alias) to the registry. The listing shows only the commands the caller's role may
// run, grouped by source, so a non-owner does not see owner-only commands.
func RegisterListing(r *Registry) {
	list := Command{
		Name:        "commands",
		Description: "List the commands you can use.",
		Source:      "builtin",
		Handler: func(_ context.Context, req Request) (string, error) {
			return r.render(req.Role), nil
		},
	}
	r.Register(list)

	help := list
	help.Name = "help"
	help.Description = "Alias for /commands."
	r.Register(help)

	// /reset and /compact act on the agent's conversation, which lives in the
	// container. They are PASS-THROUGH: the host does not execute them, it lets the
	// message flow to the runner, which intercepts them (cmd/claude-runner). They
	// register here only so /commands lists them.
	r.Register(Command{
		Name:        "reset",
		Description: "Start a fresh conversation (clear the agent's memory of this chat).",
		Source:      "builtin",
		PassThrough: true,
	})
	r.Register(Command{
		Name:        "compact",
		Description: "Compact the conversation, preserving context but shrinking it.",
		Source:      "builtin",
		PassThrough: true,
	})
}

// render formats the visible commands for a role into a chat-friendly listing,
// grouped by source (builtin first, then each plugin alphabetically).
func (r *Registry) render(role permissions.Role) string {
	cmds := r.Visible(role)
	if len(cmds) == 0 {
		return "No commands available."
	}

	// Group by source, preserving a stable order: builtin first, then others.
	groups := map[string][]Command{}
	var order []string
	for _, c := range cmds {
		if _, seen := groups[c.Source]; !seen {
			order = append(order, c.Source)
		}
		groups[c.Source] = append(groups[c.Source], c)
	}
	sortSources(order)

	var b strings.Builder
	b.WriteString("Commands:\n")
	for _, src := range order {
		if src != "" && src != "builtin" {
			fmt.Fprintf(&b, "\n%s:\n", src)
		}
		for _, c := range groups[src] {
			fmt.Fprintf(&b, "  /%s", c.Name)
			if c.Description != "" {
				fmt.Fprintf(&b, " - %s", c.Description)
			}
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func sortSources(srcs []string) {
	// builtin sorts first; the rest alphabetically.
	rank := func(s string) string {
		if s == "builtin" {
			return "\x00" // sorts before any real name
		}
		return s
	}
	for i := 1; i < len(srcs); i++ {
		for j := i; j > 0 && rank(srcs[j]) < rank(srcs[j-1]); j-- {
			srcs[j], srcs[j-1] = srcs[j-1], srcs[j]
		}
	}
}
