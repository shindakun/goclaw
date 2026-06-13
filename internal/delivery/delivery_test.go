package delivery

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/shindakun/goclaw/internal/channels"
	"github.com/shindakun/goclaw/internal/db"
	"github.com/shindakun/goclaw/internal/eventlog"
)

// fakeAdapter records what was sent and never touches a network.
type fakeAdapter struct {
	mu   sync.Mutex
	sent []channels.OutboundMsg
}

func (f *fakeAdapter) Name() string { return "telegram" }
func (f *fakeAdapter) Start(ctx context.Context) (<-chan channels.InboundMsg, error) {
	ch := make(chan channels.InboundMsg)
	close(ch)
	return ch, nil
}
func (f *fakeAdapter) Send(ctx context.Context, out channels.OutboundMsg) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, out)
	return nil
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// setup builds a central DB with one agent group + session, returns the
// deliverer, the open session, the agent group id, and the data dir.
func setup(t *testing.T) (*Deliverer, *db.DB, int64, string, *fakeAdapter) {
	t.Helper()
	dataDir := t.TempDir()
	central, err := db.Open(filepath.Join(dataDir, "central.db"))
	if err != nil {
		t.Fatalf("open central: %v", err)
	}
	t.Cleanup(func() { _ = central.Close() })

	_, agID, err := central.Apply(db.Bootstrap{
		DefaultAgentGroupName:   "default",
		DefaultAgentGroupFolder: "default",
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	fake := &fakeAdapter{}
	reg := channels.NewRegistry()
	if err := reg.Register(fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	d := New(central, reg, dataDir, nil, quiet())
	return d, central, agID, dataDir, fake
}

func TestDrain_DeliversOriginChat(t *testing.T) {
	d, central, agID, dataDir, fake := setup(t)

	// Session for telegram:555; the agent writes a reply to the origin chat.
	const key = "telegram:555"
	if _, err := central.ResolveOrCreateSession(agID, key); err != nil {
		t.Fatalf("session row: %v", err)
	}
	// The runner writes the reply: use the RUNNER opener (outbound read-write).
	runner, err := db.OpenSessionDir(db.SessionDir(dataDir, agID, key))
	if err != nil {
		t.Fatalf("open session (runner): %v", err)
	}
	if _, err := runner.EnqueueOutbound("telegram", "555", "echo: hi"); err != nil {
		t.Fatalf("enqueue outbound: %v", err)
	}
	_ = runner.Close()

	if err := d.drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if len(fake.sent) != 1 || fake.sent[0].Text != "echo: hi" {
		t.Fatalf("expected one delivered echo, got %+v", fake.sent)
	}

	// Delivery is recorded in the host-owned ledger (inbound.db), NOT by
	// mutating outbound.db. The outbound row stays 'pending' there - that file
	// belongs to the runner and the host never writes it (brief §5.1).
	sess, _ := db.OpenSession(dataDir, agID, key)
	defer func() { _ = sess.Close() }()
	delivered, err := sess.WasDelivered(1)
	if err != nil {
		t.Fatalf("WasDelivered: %v", err)
	}
	if !delivered {
		t.Fatalf("expected outbound id 1 recorded as delivered in the ledger")
	}

	// Idempotency: a second drain must NOT re-send. This is the duplicate-
	// delivery bug - the outbound row is still 'pending' in outbound.db (the
	// runner could even have rewritten it), but the ledger suppresses a re-send.
	if err := d.drain(context.Background()); err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if len(fake.sent) != 1 {
		t.Fatalf("re-drain re-sent: expected still 1 sent, got %d", len(fake.sent))
	}
}

func TestDrain_DeniesNonOriginWithoutDestination(t *testing.T) {
	d, central, agID, dataDir, fake := setup(t)

	const key = "telegram:555"
	if _, err := central.ResolveOrCreateSession(agID, key); err != nil {
		t.Fatalf("session row: %v", err)
	}
	// Runner writes the reply (outbound read-write opener).
	runner, err := db.OpenSessionDir(db.SessionDir(dataDir, agID, key))
	if err != nil {
		t.Fatalf("open session (runner): %v", err)
	}
	// Reply targets a DIFFERENT chat with no agent_destinations row → denied.
	if _, err := runner.EnqueueOutbound("telegram", "999", "leak"); err != nil {
		t.Fatalf("enqueue outbound: %v", err)
	}
	_ = runner.Close()

	if err := d.drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(fake.sent) != 0 {
		t.Fatalf("expected nothing delivered, got %+v", fake.sent)
	}
}

// fakeInterceptor rewrites a "REWRITE:<x>" marker to "<x>", and swallows "SWALLOW".
type fakeInterceptor struct{}

func (fakeInterceptor) Intercept(channel, chatID, text string) (string, bool) {
	if rest, ok := strings.CutPrefix(text, "REWRITE:"); ok {
		return rest, true
	}
	if text == "SWALLOW" {
		return "", true
	}
	return "", false
}

func enqueueAndDrain(t *testing.T, d *Deliverer, central *db.DB, agID int64, dataDir, key, text string) {
	t.Helper()
	if _, err := central.ResolveOrCreateSession(agID, key); err != nil {
		t.Fatal(err)
	}
	runner, err := db.OpenSessionDir(db.SessionDir(dataDir, agID, key))
	if err != nil {
		t.Fatal(err)
	}
	// Deliver to the session's OWN origin chat (origin-chat is always authorized).
	_, chat, _ := strings.Cut(key, ":")
	if _, err := runner.EnqueueOutbound("telegram", chat, text); err != nil {
		t.Fatal(err)
	}
	_ = runner.Close()
	if err := d.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDrain_InterceptRewritesText(t *testing.T) {
	d, central, agID, dataDir, fake := setup(t)
	d = d.WithInterceptor(fakeInterceptor{})
	enqueueAndDrain(t, d, central, agID, dataDir, "telegram:555", "REWRITE:scheduled ok")
	if len(fake.sent) != 1 || fake.sent[0].Text != "scheduled ok" {
		t.Fatalf("intercept should have rewritten the text, got %+v", fake.sent)
	}
}

func TestDrain_InterceptSwallows(t *testing.T) {
	d, central, agID, dataDir, fake := setup(t)
	d = d.WithInterceptor(fakeInterceptor{})
	enqueueAndDrain(t, d, central, agID, dataDir, "telegram:555", "SWALLOW")
	if len(fake.sent) != 0 {
		t.Fatalf("a swallowed message should not be sent, got %+v", fake.sent)
	}
	// A non-marker message passes through unchanged.
	enqueueAndDrain(t, d, central, agID, dataDir, "telegram:777", "plain reply")
	if len(fake.sent) != 1 || fake.sent[0].Text != "plain reply" {
		t.Fatalf("non-marker should pass through, got %+v", fake.sent)
	}
}

// A delivered 'turn_failed' outbound row (the runner's give-up apology) emits a
// runner.turn_failed event in addition to delivery.sent, so the introspection
// skill can see the turn failed. A normal 'reply' row emits only delivery.sent.
func TestDrain_TurnFailedEmitsEvent(t *testing.T) {
	d, central, agID, dataDir, _ := setup(t)
	evDir := t.TempDir()
	ev, err := eventlog.New(evDir, eventlog.Config{}, quiet())
	if err != nil {
		t.Fatalf("eventlog: %v", err)
	}
	d = d.WithEventLog(ev)

	const key = "telegram:555"
	if _, err := central.ResolveOrCreateSession(agID, key); err != nil {
		t.Fatalf("session: %v", err)
	}
	runner, err := db.OpenSessionDir(db.SessionDir(dataDir, agID, key))
	if err != nil {
		t.Fatalf("open runner session: %v", err)
	}
	// A normal reply and a give-up reply, in order.
	if _, err := runner.EnqueueOutbound("telegram", "555", "a normal answer"); err != nil {
		t.Fatalf("enqueue reply: %v", err)
	}
	if _, err := runner.EnqueueOutboundKind("telegram", "555", "could not reach the model", "turn_failed", "user"); err != nil {
		t.Fatalf("enqueue turn_failed: %v", err)
	}
	_ = runner.Close()

	if err := d.drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(evDir, "event-log.jsonl"))
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	log := string(data)
	// Exactly one runner.turn_failed (for the give-up row), and two delivery.sent.
	if got := strings.Count(log, `"kind":"runner.turn_failed"`); got != 1 {
		t.Fatalf("want exactly 1 runner.turn_failed, got %d; log:\n%s", got, log)
	}
	if got := strings.Count(log, `"kind":"delivery.sent"`); got != 2 {
		t.Fatalf("want 2 delivery.sent (both rows delivered), got %d; log:\n%s", got, log)
	}
}

// Delivering a turn_failed row whose source names a scheduled task re-arms it: the
// host clears that task's last-run stamp so the scheduler re-fires it (the scheduler
// stamps last-run at fire time, so without this the failed job is silently lost).
func TestDrain_TurnFailedRearmsScheduledJob(t *testing.T) {
	d, central, agID, dataDir, _ := setup(t)

	const key = "telegram:555"
	if _, err := central.ResolveOrCreateSession(agID, key); err != nil {
		t.Fatalf("session: %v", err)
	}
	// The scheduler stamped last-run at fire time.
	if err := central.SetKV("task:lastrun:job7", "2026-06-13T07:00:00Z"); err != nil {
		t.Fatalf("seed last-run: %v", err)
	}

	runner, err := db.OpenSessionDir(db.SessionDir(dataDir, agID, key))
	if err != nil {
		t.Fatalf("open runner session: %v", err)
	}
	if _, err := runner.EnqueueOutboundKind("telegram", "555", "scheduled task couldn't run", "turn_failed", "task:job7"); err != nil {
		t.Fatalf("enqueue turn_failed: %v", err)
	}
	_ = runner.Close()

	if err := d.drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	// last-run cleared => the task is due again on the next scheduler tick.
	if _, ok, err := central.GetKV("task:lastrun:job7"); err != nil || ok {
		t.Fatalf("task last-run should be cleared (re-armed); ok=%v err=%v", ok, err)
	}
}

// A normal reply (kind 'reply', source 'user') must NOT re-arm anything.
func TestDrain_NormalReplyDoesNotRearm(t *testing.T) {
	d, central, agID, dataDir, _ := setup(t)
	const key = "telegram:555"
	if _, err := central.ResolveOrCreateSession(agID, key); err != nil {
		t.Fatalf("session: %v", err)
	}
	if err := central.SetKV("task:lastrun:job7", "2026-06-13T07:00:00Z"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runner, err := db.OpenSessionDir(db.SessionDir(dataDir, agID, key))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := runner.EnqueueOutbound("telegram", "555", "a normal answer"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	_ = runner.Close()

	if err := d.drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if _, ok, _ := central.GetKV("task:lastrun:job7"); !ok {
		t.Fatal("a normal reply must not re-arm (clear) a task's last-run")
	}
}
