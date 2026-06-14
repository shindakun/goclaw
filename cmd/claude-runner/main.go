// Command claude-runner is the real in-container agent runner: it drives Claude
// (via github.com/shindakun/agent-sdk-go, which runs the `claude` CLI) for each
// inbound message and writes the reply to outbound (brief §4, Option A′).
//
// Like cmd/stub-runner it serves a whole agent group: -dir points at the group's
// sessions directory (the /sessions mount in a container). For each session it
// polls inbound.db, runs Claude on the message text, and enqueues the result to
// outbound.db. Opening the session DBs fresh each poll is deliberate - a
// long-lived SQLite handle doesn't see the host's later writes across the
// container bind mount.
//
// Requires a working `claude` CLI on PATH and ANTHROPIC_API_KEY in the env.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	claude "github.com/shindakun/agent-sdk-go"

	"github.com/shindakun/goclaw/internal/db"
)

const (
	// vaultDir is the in-container mount point of the knowledge vault (brief §11).
	vaultDir = "/vault"
	// composedPromptPath is the agent's system prompt, composed by the host into
	// claude-home before launch (see internal/runtime/compose.go): the generic
	// agent-first base plus skill symlinks. It exists whether or not a vault is
	// mounted, so the agent always has an identity - the vault is optional.
	composedPromptPath = "/home/agent/.claude/CLAUDE.md"
	// vaultMarkerPath detects a mounted vault. The librarian is a vault-provided
	// skill the host symlinks in when this is present; the runner only needs to
	// know a vault exists to add the absolute-path note below.
	vaultMarkerPath = vaultDir + "/CLAUDE.md"
	// workDir is the agent's scratch working directory (clones, temp files), kept
	// separate from /vault so command output never pollutes the vault.
	workDir = "/work"
)

func main() {
	dir := flag.String("dir", "/sessions", "agent-group sessions directory (parent of per-session subdirs)")
	model := flag.String("model", "", "Claude model id (empty = CLI default)")
	systemPromptFile := flag.String("system-prompt-file", "", "path to a system prompt file (e.g. the vault's CLAUDE.md)")
	once := flag.Bool("once", false, "process the current backlog once and exit")
	interval := flag.Duration("interval", 1*time.Second, "poll interval")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Detect a mounted vault (the librarian skill is symlinked in by the host when
	// one is present; the runner only needs this to add the vault-path note).
	vaultMounted := false
	if _, err := os.Stat(vaultMarkerPath); err == nil {
		vaultMounted = true
	}
	// Default the system prompt to the host-composed CLAUDE.md (base + skills),
	// which exists whether or not a vault is mounted. An explicit -system-prompt-file
	// still wins (e.g. running the runner standalone in dev).
	promptFile := *systemPromptFile
	if promptFile == "" {
		if _, err := os.Stat(composedPromptPath); err == nil {
			promptFile = composedPromptPath
			log.Info("using composed system prompt", "path", promptFile)
		}
	}

	// Model: the explicit -model flag wins (dev/standalone); otherwise fall back to
	// GOCLAW_MODEL, which the host injects from the agent spec's Model layer. Empty
	// leaves the CLI default.
	modelID := *model
	if modelID == "" {
		modelID = os.Getenv("GOCLAW_MODEL")
	}

	r := &runner{
		model:            modelID,
		systemPromptFile: promptFile,
		vaultMounted:     vaultMounted,
		rotate:           loadRotateConfig(),
		log:              log,
	}
	// Discover and launch plugins INSIDE the container (untrusted code stays in the
	// sandbox, never on the host). Their tools are exposed to the agent as local MCP
	// tools. nil when there are no plugins.
	r.plugins = startPlugins(context.Background(), pluginsDir, log)
	defer r.plugins.Close()

	if err := r.run(*dir, *once, *interval); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

type runner struct {
	model            string
	systemPromptFile string
	vaultMounted     bool
	rotate           rotateConfig
	plugins          *pluginHost // in-container plugin tools (nil when none)
	log              *slog.Logger
}

func (r *runner) run(sessionsDir string, once bool, interval time.Duration) error {
	// Fail fast on a missing/mismatched CLI so the container doesn't silently
	// loop without ever being able to answer.
	if v, err := claude.CheckCLIVersion(context.Background(), ""); err != nil {
		r.log.Warn("claude CLI check failed - replies will error until fixed", "err", err)
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

// maxTransientAttempts bounds how many times one inbound message is retried after a
// transient (infrastructure) failure before the runner gives up. A brief blip
// clears within a poll or two; a sustained failure (API out of tokens, a hard
// upstream error) never will, so after this many tries the runner stops deferring,
// reports the failure to the chat, and consumes the message instead of refreshing a
// stuck typing indicator forever. The retry cadence is the runner's poll interval.
const maxTransientAttempts = 5

// metaAttemptsPrefix keys the per-inbound-message transient-failure counter in the
// runner-owned outbound.db meta KV (so it never touches the host-owned inbound.db,
// preserving single-writer-per-file). The full key is metaAttemptsPrefix + <id>.
const metaAttemptsPrefix = "attempts:"

// metaSessionID is the session-DB meta key under which we persist Claude's
// conversation session id for multi-turn continuity.
const metaSessionID = "claude_session_id"

// autoCompactInputTokens is the resumed-turn input-token threshold above which
// the runner compacts the conversation on the NEXT turn, to keep the context
// window from filling. Conservative; the CLI also compacts on its own near the
// hard limit.
const autoCompactInputTokens = 150_000

// msgHandler processes one message, returning its reply and a non-nil transient error if
// the turn could not complete for an infrastructure reason (see handle). r.handle is the
// production implementation; a test can substitute one.
type msgHandler func(ctx context.Context, sess *db.SessionDBs, tag, text string) (string, error)

// processSession opens one session fresh and answers each pending inbound
// message with Claude, threading multi-turn context via a stored session id.
func (r *runner) processSession(ctx context.Context, dir string) (int, error) {
	return r.processSessionWith(ctx, dir, r.handle)
}

// processSessionWith is processSession with an injectable per-message handler (for tests).
func (r *runner) processSessionWith(ctx context.Context, dir string, handle msgHandler) (int, error) {
	sess, err := db.OpenSessionDir(dir)
	if err != nil {
		return 0, err
	}
	defer func() { _ = sess.Close() }()

	pending, err := sess.PendingInbound()
	if err != nil {
		return 0, err
	}
	answered := 0
	for _, m := range pending {
		reply, transient := handle(ctx, sess, filepath.Base(dir), m.Text)
		if transient != nil {
			// Infrastructure failure (network/API/CLI). Retry it a bounded number of
			// times by leaving it (and the rest of this session's pending, processed in
			// order) queued so the next poll re-runs it once the cause clears. A brief
			// blip recovers within a poll or two; this is the fix for "a scheduled task
			// fired during an outage, errored, and was lost".
			attempts := r.bumpAttempts(sess, m.ID)
			if attempts < maxTransientAttempts {
				r.log.Warn("deferring message for retry (transient failure)",
					"session", filepath.Base(dir), "in_id", m.ID, "attempt", attempts,
					"max", maxTransientAttempts, "err", transient)
				break // stop this session's pass; later messages stay queued, order preserved
			}
			// Out of retries: the failure is not clearing (e.g. API out of tokens). Stop
			// deferring forever. Report it to the chat (this reaches the operator AND, via
			// the normal delivery path, stops the stuck "typing" indicator) and CONSUME the
			// message so it does not loop. Better a visible failure than a silent hang.
			r.log.Error("giving up on message after repeated transient failures",
				"session", filepath.Base(dir), "in_id", m.ID, "source", m.Source, "attempts", attempts, "err", transient)
			// Enqueue as kind 'turn_failed' so the host emits a runner.turn_failed event
			// on delivery (the introspection skill can spot a run of these), not just a
			// normal reply. Echo m.Source so the host can re-arm a failed scheduled job
			// (clear its last-run so it re-fires) instead of leaving it silently lost. The
			// notice text is tailored to the origin so a scheduled-job failure does not look
			// like a reply to a message the user never sent.
			if _, err := sess.EnqueueOutboundKind(m.Channel, m.ChatID,
				giveUpNotice(m.Source), "turn_failed", m.Source); err != nil {
				return answered, err
			}
			if err := sess.SetInboundHWM(m.ID); err != nil {
				return answered, err
			}
			r.clearAttempts(sess, m.ID)
			continue
		}
		if _, err := sess.EnqueueOutbound(m.Channel, m.ChatID, reply); err != nil {
			return answered, err
		}
		// Advance the processed high-water mark in outbound.db (the runner's own
		// DB). The runner never writes inbound.db, so the host's inbound inserts
		// can't be clobbered (brief §5.1, single-writer per DB).
		if err := sess.SetInboundHWM(m.ID); err != nil {
			return answered, err
		}
		// Succeeded: drop any transient-failure count this message accrued, so a later
		// message that briefly fails starts from a clean attempt count.
		r.clearAttempts(sess, m.ID)
		answered++
		r.log.Info("answered", "session", filepath.Base(dir), "in_id", m.ID)
	}
	return answered, nil
}

// giveUpNotice composes the user-facing give-up text for a message the runner could
// not answer, tailored to the message's origin so a scheduled-job failure does not
// read as a reply to a message the user never sent. source is the inbound source:
// "user", "task:<id>", or "maint:<name>".
func giveUpNotice(source string) string {
	const tail = " (the API may be down or out of quota)."
	switch {
	case strings.HasPrefix(source, "maint:"):
		return "⚠️ Scheduled maintenance job \"" + strings.TrimPrefix(source, "maint:") +
			"\" couldn't run: I couldn't reach the model after several tries" + tail +
			" It will be retried shortly."
	case strings.HasPrefix(source, "task:"):
		// The task id is an opaque identifier, not friendly; say "a scheduled task" and
		// let the re-arm + the host's event log carry the specifics.
		return "⚠️ A scheduled task couldn't run: I couldn't reach the model after several tries" +
			tail + " It will be retried shortly."
	default:
		return "⚠️ I couldn't reach the model after several tries" + tail +
			" Your message wasn't answered; please try again later."
	}
}

// bumpAttempts increments and returns the transient-failure count for an inbound
// message, persisted in the runner-owned meta KV so it survives across polls (and
// across a container restart). A read/parse failure is treated as "this is attempt
// 1" rather than blocking the turn on a counter.
func (r *runner) bumpAttempts(sess *db.SessionDBs, inboundID int64) int {
	key := metaAttemptsPrefix + strconv.FormatInt(inboundID, 10)
	n := 0
	if v, ok, err := sess.GetMeta(key); err == nil && ok {
		if parsed, perr := strconv.Atoi(v); perr == nil {
			n = parsed
		}
	}
	n++
	if err := sess.SetMeta(key, strconv.Itoa(n)); err != nil {
		r.log.Warn("persist attempt count", "in_id", inboundID, "err", err)
	}
	return n
}

// clearAttempts removes a message's transient-failure counter once it is resolved
// (answered or given up on), so the KV does not accumulate stale keys.
func (r *runner) clearAttempts(sess *db.SessionDBs, inboundID int64) {
	if err := sess.DeleteMeta(metaAttemptsPrefix + strconv.FormatInt(inboundID, 10)); err != nil {
		r.log.Warn("clear attempt count", "in_id", inboundID, "err", err)
	}
}

// handle processes one message: intercepts /reset and /compact commands,
// otherwise asks Claude with conversation continuity.
//
// It returns (reply, transient). A nil transient means the message is DONE (the reply, even
// an unhappy one, is the real outcome and the message should be consumed). A non-nil
// transient means the turn could not complete for an INFRASTRUCTURE reason (the claude CLI
// failed: network/API unreachable, a crashed subprocess). In that case the caller must NOT
// consume the message and NOT deliver a reply: the inbound stays queued and is retried when
// the runner next processes the session (e.g. after the network recovers). This is the fix
// for "a scheduled morning task fired during a network outage, errored, and was lost": a
// transient failure must not look like a completed turn.
func (r *runner) handle(ctx context.Context, sess *db.SessionDBs, tag, text string) (string, error) {
	switch strings.TrimSpace(text) {
	case "/reset":
		if err := sess.DeleteMeta(metaSessionID); err != nil {
			return "⚠️ reset failed: " + err.Error(), nil
		}
		r.log.Info("session reset", "session", tag)
		return "🧹 Conversation reset. Starting fresh.", nil
	case "/compact":
		if err := r.compact(ctx, sess, tag); err != nil {
			return "⚠️ compact failed: " + err.Error(), nil
		}
		return "🗜️ Conversation compacted; context preserved.", nil
	}

	// A slash command matching a loaded plugin tool is dispatched DIRECTLY to that
	// plugin (no LLM turn), so /roll 2d6 is a fast, deterministic call. Plugins run
	// in this container, so this is a local invoke. Unrecognized slashes fall
	// through to the agent (which may treat them as instructions).
	if reply, ok := r.plugins.command(ctx, text); ok {
		return reply, nil
	}

	reply, err := r.ask(ctx, sess, tag, text)
	if err != nil {
		// Infrastructure failure: surface it as a TRANSIENT error so the caller leaves the
		// message queued for retry instead of consuming it and delivering an error string.
		r.log.Error("claude query", "session", tag, "err", err)
		return "", fmt.Errorf("transient agent failure: %w", err)
	}
	return reply, nil
}

// ask runs Claude on one prompt, resuming the stored session id for continuity,
// persists the (possibly new) session id, and auto-compacts when the context
// grows large. Returns the final reply text. If the stored session id can't be
// resumed (its conversation no longer exists in this container - e.g. after a
// container restart), the id is dropped and the turn retried fresh.
func (r *runner) ask(ctx context.Context, sess *db.SessionDBs, tag, prompt string) (string, error) {
	resumeID, _, _ := sess.GetMeta(metaSessionID)

	// Rotation guard: if the resume transcript has grown too big or too old to
	// reload within the host's idle ceiling, move it aside and start fresh -
	// otherwise the cold container hangs reloading it and gets killed before it
	// can reply (see rotate.go). Common case: no transcript over the cap, no-op.
	if resumeID != "" {
		if reason := r.maybeRotateTranscript(resumeID); reason != "" {
			r.log.Info("rotating session - "+reason+"; starting fresh", "session", tag)
			_ = sess.DeleteMeta(metaSessionID)
			resumeID = ""
		}
	}

	result, sessionID, inputTokens, err := r.query(ctx, resumeID, prompt)
	if errors.Is(err, errStaleResume) {
		r.log.Info("stale session id - starting fresh", "session", tag)
		_ = sess.DeleteMeta(metaSessionID)
		result, sessionID, inputTokens, err = r.query(ctx, "", prompt)
	}
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
	_, newID, _, err := r.query(ctx, id, "/compact")
	if errors.Is(err, errStaleResume) {
		// The conversation is gone; nothing to compact. Forget it.
		_ = sess.DeleteMeta(metaSessionID)
		return nil
	}
	if err != nil {
		return err
	}
	if newID != "" {
		_ = sess.SetMeta(metaSessionID, newID)
	}
	r.log.Info("compacted", "session", tag)
	return nil
}

// errStaleResume indicates the stored session id couldn't be resumed (its
// conversation no longer exists in this container - e.g. after a container
// restart). The caller drops the id and retries fresh.
var errStaleResume = errors.New("stale resume session id")

// query runs one Claude turn. resumeID continues an existing conversation when
// non-empty; "" starts fresh. Returns the result text, the resulting session
// id, and the turn's input token count. A failed resume returns errStaleResume.
func (r *runner) query(ctx context.Context, resumeID, prompt string) (result, sessionID string, inputTokens int, err error) {
	opts := []claude.Option{}
	if r.model != "" {
		opts = append(opts, claude.WithModel(r.model))
	}
	if r.systemPromptFile != "" {
		if _, statErr := os.Stat(r.systemPromptFile); statErr == nil {
			opts = append(opts, claude.WithSystemPromptFile(r.systemPromptFile))
		}
	}
	// Harness invariants: the always-on instruction blocks (vault-path discipline,
	// authoritative current time) appended every turn. Their TEXT is host-owned data
	// (cmd/claude-runner/harness.go); the live per-turn VALUES are substituted here.
	// The vault-path block is gated on a mounted vault, mirroring the prior behavior;
	// the current time is computed per turn so it is always fresh, and the agent must
	// use THIS rather than guessing (it otherwise produces invalid stamps like 24:30).
	now := time.Now()
	invariants := harnessSpec().RenderInvariants(r.vaultMounted, map[string]string{
		"vault_dir": vaultDir,
		"work_dir":  workDir,
		"now":       now.Format("2006-01-02 15:04 MST"),
	})
	for _, text := range invariants {
		opts = append(opts, claude.WithAppendSystemPrompt(text))
	}
	if r.vaultMounted {
		// Grant the agent access to the vault root (cwd is /work, not the vault).
		opts = append(opts, claude.WithAddDir(vaultDir))
	}

	// Headless: there is no human at a terminal to answer tool-permission
	// prompts, so a prompting mode would hang the agent (e.g. on a `git clone`
	// or any Bash command) with no way for the user to approve. Bypass prompts
	// entirely. This is safe because the CONTAINER is the security sandbox: the
	// agent can only reach its mounts (/sessions, /vault, ~/.claude, /work) and
	// the network, never anything on the host (brief §9).
	opts = append(opts, claude.WithPermissionMode(claude.PermissionBypass))

	// Expose plugin tools to the agent as local MCP tools (the plugins run in THIS
	// container; nothing crosses back to the host). Only when plugins are present.
	if r.plugins.hasServerTools() {
		opts = append(opts, claude.WithSDKMCPServer(r.plugins.server.Name, r.plugins.server))
	}

	// Work in /work, NOT /vault: scratch like cloned repos and temp files must
	// not pollute the vault. The vault stays a known location at /vault that the
	// agent reads/writes by absolute path (see the appended system prompt above).
	//
	// TRIPWIRE: the CLI derives its auto-memory directory key from this cwd
	// (~/.claude/projects/<mangled-cwd>/memory). Keeping cwd stable at /work is
	// what makes the agent's durable memory accumulate in ONE place across runs.
	// Changing this cwd silently orphans all prior auto-memory under the old key.
	// If you ever change it, migrate the memory dir too.
	opts = append(opts, claude.WithCwd(workDir))
	if resumeID != "" {
		opts = append(opts, claude.WithResume(resumeID))
	}

	// Archive the conversation to conversations/ just before the CLI compacts it,
	// so the about-to-be-summarized history stays recallable (base memory, always
	// on - see archive.go). The hook payload carries the transcript path.
	hooks := map[claude.HookEvent][]claude.HookMatcher{
		claude.HookPreCompact: {{Callbacks: []claude.HookCallback{r.preCompactArchive}}},
	}
	// When a vault is mounted, re-assert the vault-first discipline on EVERY turn via a
	// UserPromptSubmit hook. A reminder buried in the system prompt gets scrolled past; a
	// hook injects it fresh at decision time each message. Registered programmatically here
	// (not via ~/.claude/settings.json) so it ships in the runner and every install/agent
	// group gets it by construction, no per-container hand-editing that a fresh install
	// would lose. additionalContext is the documented field that reaches the model's
	// context for UserPromptSubmit.
	if r.vaultMounted {
		hooks[claude.HookUserPromptSubmit] = []claude.HookMatcher{
			{Callbacks: []claude.HookCallback{r.vaultFirstReminder}},
		}
	}
	opts = append(opts, claude.WithHooks(hooks))

	for msg, qErr := range claude.Query(ctx, prompt, opts...) {
		if qErr != nil {
			// The CLI subprocess failed. A failed --resume often surfaces here as
			// "connection closed" (the CLI exits before the control handshake).
			if resumeID != "" {
				return "", "", 0, errStaleResume
			}
			var pe *claude.ProcessError
			if errors.As(qErr, &pe) && strings.TrimSpace(pe.Stderr) != "" {
				return "", "", 0, fmt.Errorf("claude CLI: %s", firstLine(pe.Stderr))
			}
			return "", "", 0, qErr
		}
		if res, ok := msg.(*claude.ResultMessage); ok {
			if res.IsError {
				// "No conversation found with session ID …" from a stale resume.
				if resumeID != "" && isNoConversation(res) {
					return "", "", 0, errStaleResume
				}
				return "", "", 0, resultError(res)
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

// isNoConversation reports whether an error result is the CLI's "No conversation
// found with session ID …" - i.e. a stale --resume.
func isNoConversation(res *claude.ResultMessage) bool {
	hay := strings.ToLower(strings.Join(res.Errors, " ") + " " + res.Result + " " + string(res.Raw))
	return strings.Contains(hay, "no conversation found")
}

// resultError builds the most informative error from an error ResultMessage.
// The CLI doesn't always populate Errors, so fall back through APIErrorStatus,
// Result (which often carries the message), Subtype, then the raw payload.
func resultError(res *claude.ResultMessage) error {
	if len(res.Errors) > 0 {
		return fmt.Errorf("claude: %s", strings.Join(res.Errors, "; "))
	}
	status := ""
	if res.APIErrorStatus != nil {
		status = fmt.Sprintf("API %d: ", *res.APIErrorStatus)
	}
	if msg := strings.TrimSpace(res.Result); msg != "" {
		return fmt.Errorf("claude: %s%s", status, firstLine(msg))
	}
	if res.Subtype != "" {
		return fmt.Errorf("claude: %s%s", status, res.Subtype)
	}
	if status != "" {
		return fmt.Errorf("claude: %serror", status)
	}
	if raw := strings.TrimSpace(string(res.Raw)); raw != "" {
		return fmt.Errorf("claude: %s", firstLine(raw))
	}
	return fmt.Errorf("claude: result reported an error")
}

// firstLine returns the first non-empty line of s, trimmed, capped to keep chat
// replies readable.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if len(line) > 300 {
				return line[:300] + "…"
			}
			return line
		}
	}
	return strings.TrimSpace(s)
}
