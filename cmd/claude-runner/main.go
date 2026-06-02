// Command claude-runner is the real in-container agent runner: it drives Claude
// (via github.com/shindakun/agent-sdk-go, which runs the `claude` CLI) for each
// inbound message and writes the reply to outbound (brief §4, Option A′).
//
// Like cmd/stub-runner it serves a whole agent group: -dir points at the group's
// sessions directory (the /sessions mount in a container). For each session it
// polls inbound.db, runs Claude on the message text, and enqueues the result to
// outbound.db. Opening the session DBs fresh each poll is deliberate — a
// long-lived SQLite handle doesn't see the host's later writes across the
// container bind mount.
//
// Requires a working `claude` CLI on PATH and ANTHROPIC_API_KEY in the env.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	claude "github.com/shindakun/agent-sdk-go"

	"github.com/shindakun/goclaw/internal/db"
)

func main() {
	dir := flag.String("dir", "/sessions", "agent-group sessions directory (parent of per-session subdirs)")
	model := flag.String("model", "", "Claude model id (empty = CLI default)")
	systemPromptFile := flag.String("system-prompt-file", "", "path to a system prompt file (e.g. the group's CLAUDE.md)")
	once := flag.Bool("once", false, "process the current backlog once and exit")
	interval := flag.Duration("interval", 1*time.Second, "poll interval")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	r := &runner{
		model:            *model,
		systemPromptFile: *systemPromptFile,
		log:              log,
	}
	if err := r.run(*dir, *once, *interval); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

type runner struct {
	model            string
	systemPromptFile string
	log              *slog.Logger
}

func (r *runner) run(sessionsDir string, once bool, interval time.Duration) error {
	// Fail fast on a missing/mismatched CLI so the container doesn't silently
	// loop without ever being able to answer.
	if v, err := claude.CheckCLIVersion(context.Background(), ""); err != nil {
		r.log.Warn("claude CLI check failed — replies will error until fixed", "err", err)
	} else {
		r.log.Info("claude-runner started", "sessions_dir", sessionsDir, "cli_version", v)
	}

	if once {
		n, err := r.processAll(context.Background(), sessionsDir)
		if err != nil {
			return err
		}
		r.log.Info("done", "answered", n)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.log.Info("claude-runner stopped")
			return nil
		case <-ticker.C:
			if _, err := r.processAll(ctx, sessionsDir); err != nil {
				r.log.Error("process", "err", err)
			}
		}
	}
}

// processAll scans the sessions directory for session subdirs and processes
// each. Returns the total number of messages answered.
func (r *runner) processAll(ctx context.Context, sessionsDir string) (int, error) {
	entries, err := os.ReadDir(sessionsDir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	total := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(sessionsDir, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "inbound.db")); err != nil {
			continue
		}
		n, err := r.processSession(ctx, dir)
		if err != nil {
			r.log.Error("process session", "session", e.Name(), "err", err)
			continue
		}
		total += n
	}
	return total, nil
}

// metaSessionID is the session-DB meta key under which we persist Claude's
// conversation session id for multi-turn continuity.
const metaSessionID = "claude_session_id"

// autoCompactInputTokens is the resumed-turn input-token threshold above which
// the runner compacts the conversation on the NEXT turn, to keep the context
// window from filling. Conservative; the CLI also compacts on its own near the
// hard limit.
const autoCompactInputTokens = 150_000

// processSession opens one session fresh and answers each pending inbound
// message with Claude, threading multi-turn context via a stored session id.
func (r *runner) processSession(ctx context.Context, dir string) (int, error) {
	sess, err := db.OpenSessionDir(dir)
	if err != nil {
		return 0, err
	}
	defer sess.Close()

	pending, err := sess.PendingInbound()
	if err != nil {
		return 0, err
	}
	for _, m := range pending {
		reply := r.handle(ctx, sess, filepath.Base(dir), m.Text)
		if _, err := sess.EnqueueOutbound(m.Channel, m.ChatID, reply); err != nil {
			return 0, err
		}
		if err := sess.MarkInboundConsumed(m.ID); err != nil {
			return 0, err
		}
		r.log.Info("answered", "session", filepath.Base(dir), "in_id", m.ID)
	}
	return len(pending), nil
}

// handle processes one message: intercepts /reset and /compact commands,
// otherwise asks Claude with conversation continuity. Always returns a
// user-facing reply (errors are surfaced, not swallowed).
func (r *runner) handle(ctx context.Context, sess *db.SessionDBs, tag, text string) string {
	switch strings.TrimSpace(text) {
	case "/reset":
		if err := sess.DeleteMeta(metaSessionID); err != nil {
			return "⚠️ reset failed: " + err.Error()
		}
		r.log.Info("session reset", "session", tag)
		return "🧹 Conversation reset. Starting fresh."
	case "/compact":
		if err := r.compact(ctx, sess, tag); err != nil {
			return "⚠️ compact failed: " + err.Error()
		}
		return "🗜️ Conversation compacted; context preserved."
	}

	reply, err := r.ask(ctx, sess, tag, text)
	if err != nil {
		r.log.Error("claude query", "session", tag, "err", err)
		return "⚠️ agent error: " + err.Error()
	}
	return reply
}

// ask runs Claude on one prompt, resuming the stored session id for continuity,
// persists the (possibly new) session id, and auto-compacts when the context
// grows large. Returns the final reply text.
func (r *runner) ask(ctx context.Context, sess *db.SessionDBs, tag, prompt string) (string, error) {
	result, sessionID, inputTokens, err := r.query(ctx, sess, prompt)
	if err != nil {
		return "", err
	}
	if sessionID != "" {
		if err := sess.SetMeta(metaSessionID, sessionID); err != nil {
			r.log.Warn("persist session id failed", "session", tag, "err", err)
		}
	}
	// Auto-compact on the next opportunity if this turn's context is large.
	if inputTokens >= autoCompactInputTokens {
		r.log.Info("auto-compacting (high context)", "session", tag, "input_tokens", inputTokens)
		if err := r.compact(ctx, sess, tag); err != nil {
			r.log.Warn("auto-compact failed", "session", tag, "err", err)
		}
	}
	return result, nil
}

// compact asks Claude to summarize and shrink the current conversation while
// keeping the thread (the CLI's /compact). The session id is preserved.
func (r *runner) compact(ctx context.Context, sess *db.SessionDBs, tag string) error {
	id, ok, err := sess.GetMeta(metaSessionID)
	if err != nil {
		return err
	}
	if !ok || id == "" {
		return nil // nothing to compact yet
	}
	_, newID, _, err := r.query(ctx, sess, "/compact")
	if err != nil {
		return err
	}
	if newID != "" {
		_ = sess.SetMeta(metaSessionID, newID)
	}
	r.log.Info("compacted", "session", tag)
	return nil
}

// query runs one Claude turn (resuming the stored session id if present) and
// returns the final result text, the resulting session id, and the turn's input
// token count.
func (r *runner) query(ctx context.Context, sess *db.SessionDBs, prompt string) (result, sessionID string, inputTokens int, err error) {
	opts := []claude.Option{}
	if r.model != "" {
		opts = append(opts, claude.WithModel(r.model))
	}
	if r.systemPromptFile != "" {
		if _, statErr := os.Stat(r.systemPromptFile); statErr == nil {
			opts = append(opts, claude.WithSystemPromptFile(r.systemPromptFile))
		}
	}
	if id, ok, _ := sess.GetMeta(metaSessionID); ok && id != "" {
		opts = append(opts, claude.WithResume(id))
	}

	for msg, qErr := range claude.Query(ctx, prompt, opts...) {
		if qErr != nil {
			return "", "", 0, qErr
		}
		if res, ok := msg.(*claude.ResultMessage); ok {
			if res.IsError {
				if len(res.Errors) > 0 {
					return "", "", 0, fmt.Errorf("claude: %s", res.Errors[0])
				}
				return "", "", 0, fmt.Errorf("claude: result reported an error")
			}
			result = res.Result
			sessionID = res.SessionID
			inputTokens = res.Usage.InputTokens
		}
	}
	if result == "" {
		return "", sessionID, inputTokens, fmt.Errorf("claude: no result produced")
	}
	return result, sessionID, inputTokens, nil
}
