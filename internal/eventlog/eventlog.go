// Package eventlog is a host-owned, append-only, structured record of OPERATIONAL
// events: what the host actually DID (a scheduled task fired or was deferred, a
// message was delivered or denied, the proxy minted a new CA, a runner relaunched).
// It is the retained, queryable counterpart to the host's ephemeral slog stderr,
// which is thrown away when the process or container goes. Several incidents this
// project has hit (a task that fired during an outage and was silently lost; the
// proxy CA drifting and flooding bad-cert errors) are exactly the class this exists
// to make a grep instead of an archaeology dig.
//
// Shape: a single global host-side log under the data dir, one writer, append
// JSON-lines, rotated by size and age. It is mounted READ-ONLY into the runner
// container (gated on a single agent group; see cmd/goclaw) so the agent's
// introspection skill can read it. Because an untrusted agent reads it, every event
// kind must be vetted to never carry a secret or raw user content (the fields here
// are ids, names, and reasons, never tokens or message bodies).
//
// Containment note: the log is host-writes only. Nothing here gives the container a
// way to write it, which is why it is safe to mount read-only. Adding an
// agent->host "emit event" path would be a write channel out of the box and is the
// surface the boundary refuses; do not add one. (The runner signals a turn it gave
// up on by writing a 'turn_failed'-kind row to its OWN outbound.db; the HOST reads
// that on delivery and emits runner.turn_failed. The runner never writes this log.)
package eventlog

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Kind is a dotted operational event namespace. Keep the set small and curated: a
// typed constant per kind (not free-form strings) so the kinds do not drift and a
// future reader can switch on them. Add a constant here when wiring a new call site.
type Kind string

const (
	KindScheduleFired  Kind = "schedule.fired"     // a scheduled task was handed off to its runner
	KindScheduleDefer  Kind = "schedule.deferred"  // a due task could not be handed off (re-fires next tick)
	KindDeliverySent   Kind = "delivery.sent"      // an outbound message was delivered to a channel
	KindDeliveryDenied Kind = "delivery.denied"    // an outbound message was denied by authorization
	KindDeliveryFailed Kind = "delivery.failed"    // an outbound dispatch errored
	KindProxyCANew     Kind = "proxy.ca_generated" // a NEW proxy CA identity was minted (containers trust a stale cert)
	KindPluginInstall  Kind = "plugin.install"     // a plugin was installed/updated (subsumes the old .install-log.jsonl)
	KindPluginRemove   Kind = "plugin.remove"      // a plugin was removed
	KindRunnerLaunched Kind = "runner.launched"    // a runner container was (re)launched for a group
	KindRunnerReaped   Kind = "runner.reaped"      // an idle runner container was stopped/removed by the sweep
	KindRunnerTurnFail Kind = "runner.turn_failed" // the runner gave up on a message after repeated transient failures
	KindChannelAttach  Kind = "channel.attached"   // a channel plugin dialed in and its relay is live
	KindChannelDetach  Kind = "channel.detached"   // a channel plugin's connection dropped (awaiting re-dial)
	KindMaintFired     Kind = "maintenance.fired"  // a scheduled vault-maintenance job fired
)

// Event is one record. Common fields are explicit; everything kind-specific rides in
// Fields so the schema stays stable while individual kinds carry what they need. Ok
// is a pointer so a kind with no success/failure split simply omits it.
type Event struct {
	TS     string         `json:"ts"`               // RFC3339 local
	Kind   Kind           `json:"kind"`             // dotted namespace, see the Kind constants
	Ok     *bool          `json:"ok,omitempty"`     // success/failure where that split is meaningful
	Fields map[string]any `json:"fields,omitempty"` // kind-specific detail (never secrets/raw content)
}

// Config bounds the on-disk log. Zero values fall back to the defaults in New.
type Config struct {
	MaxBytes int64         // rotate when the active file exceeds this
	MaxAge   time.Duration // rotate when the active file's first event is older than this (0 disables)
}

const (
	// defaultMaxBytes keeps a long-running host's event log small. The records are
	// tiny one-liners, so a few MB is a lot of history.
	defaultMaxBytes int64 = 4 * 1024 * 1024
	// defaultMaxAge is the secondary age trigger from the active file's first event.
	defaultMaxAge = 30 * 24 * time.Hour
	// logName / rotatedName: one active file plus a single rotated generation, so the
	// log occupies at most ~2x MaxBytes. Oldest is overwritten on the next rotation
	// (we keep one generation, not an unbounded ring).
	logName     = "event-log.jsonl"
	rotatedName = "event-log.1.jsonl"
)

// Logger is the single writer. It is host-process-local, so the single-writer-per-file
// rule is satisfied trivially (no cross-mount SQLite hazards apply; this is a plain
// host file the host appends to). A mutex serializes concurrent Emit calls from the
// host's several goroutines (scheduler, delivery, proxy).
type Logger struct {
	mu      sync.Mutex
	dir     string
	cfg     Config
	now     func() time.Time // injectable for tests
	slog    *slog.Logger     // for reporting the logger's OWN failures, never the events
	started time.Time        // first-event time of the active file, for the age trigger (zero until known)
}

// New creates a Logger writing under dir (created if absent). A nil slog is tolerated
// (failures are then silent); pass the host logger so a broken event log is visible.
func New(dir string, cfg Config, log *slog.Logger) (*Logger, error) {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxBytes
	}
	if cfg.MaxAge == 0 {
		cfg.MaxAge = defaultMaxAge
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("eventlog: create dir %q: %w", dir, err)
	}
	return &Logger{dir: dir, cfg: cfg, now: time.Now, slog: log}, nil
}

// Emit appends one event. Best-effort by design: emitting an operational record must
// never fail the operation it records, so a write error is reported via slog (if set)
// and swallowed, exactly as the install log does. ok may be nil for kinds with no
// success/failure split. fields must never contain secrets or raw user content.
func (l *Logger) Emit(kind Kind, ok *bool, fields map[string]any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.maybeRotate(now)

	ev := Event{TS: now.Format(time.RFC3339), Kind: kind, Ok: ok, Fields: fields}
	line, err := json.Marshal(ev)
	if err != nil {
		l.warn("marshal event", kind, err)
		return
	}
	f, err := os.OpenFile(l.activePath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		l.warn("open event log", kind, err)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(line, '\n')); err != nil {
		l.warn("write event", kind, err)
		return
	}
	if l.started.IsZero() {
		l.started = now
	}
}

// maybeRotate moves the active file aside (overwriting the single rotated generation)
// when it has grown past MaxBytes or its first event is older than MaxAge. Caller holds
// the mutex. Rotation failure is non-fatal: we keep appending to the active file rather
// than lose the event.
//
// The age window (started) is in-memory, so it resets to zero on host restart: a stale
// file carried across a restart is not age-rotated until its window is re-established by
// the first post-restart write. Size still bounds it regardless, so this only delays an
// age-rotation, it never lets the log grow unbounded. Good enough for this slice.
func (l *Logger) maybeRotate(now time.Time) {
	path := l.activePath()
	info, err := os.Stat(path)
	if err != nil {
		l.started = time.Time{} // no active file; next write starts a fresh age window
		return
	}
	rotate := info.Size() > l.cfg.MaxBytes
	if !rotate && l.cfg.MaxAge > 0 && !l.started.IsZero() {
		rotate = now.Sub(l.started) > l.cfg.MaxAge
	}
	if !rotate {
		return
	}
	if err := os.Rename(path, filepath.Join(l.dir, rotatedName)); err != nil {
		l.warn("rotate event log", "", err)
		return
	}
	l.started = time.Time{} // the new active file's age window restarts on its first write
}

func (l *Logger) activePath() string { return filepath.Join(l.dir, logName) }

// warn reports a failure of the logger ITSELF (never an event's content) via slog.
func (l *Logger) warn(msg string, kind Kind, err error) {
	if l.slog == nil {
		return
	}
	l.slog.Warn("eventlog: "+msg, "kind", string(kind), "err", err)
}

// Bool is a helper for the *bool Ok field: eventlog.Bool(true).
func Bool(b bool) *bool { return &b }
