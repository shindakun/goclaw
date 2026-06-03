package typing

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shindakun/goclaw/internal/channels"
)

// fakeActionAdapter implements ChannelAdapter + ActionSender and counts actions.
type fakeActionAdapter struct {
	actions atomic.Int64
}

func (f *fakeActionAdapter) Name() string { return "telegram" }
func (f *fakeActionAdapter) Start(ctx context.Context) (<-chan channels.InboundMsg, error) {
	ch := make(chan channels.InboundMsg)
	close(ch)
	return ch, nil
}
func (f *fakeActionAdapter) Send(ctx context.Context, out channels.OutboundMsg) error { return nil }
func (f *fakeActionAdapter) SendAction(ctx context.Context, chatID, kind string) error {
	f.actions.Add(1)
	return nil
}

// plainAdapter implements ChannelAdapter only (no ActionSender).
type plainAdapter struct{}

func (plainAdapter) Name() string { return "plain" }
func (plainAdapter) Start(ctx context.Context) (<-chan channels.InboundMsg, error) {
	ch := make(chan channels.InboundMsg)
	close(ch)
	return ch, nil
}
func (plainAdapter) Send(ctx context.Context, out channels.OutboundMsg) error { return nil }

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestManager_StartSendsActionAndStop(t *testing.T) {
	fa := &fakeActionAdapter{}
	reg := channels.NewRegistry()
	if err := reg.Register(fa); err != nil {
		t.Fatalf("register: %v", err)
	}
	m := New(reg, quiet())

	m.Start(context.Background(), "telegram", "42")
	// The first action is sent immediately on Start.
	waitFor(t, func() bool { return fa.actions.Load() >= 1 }, time.Second)

	m.Stop("telegram", "42")
	// After Stop, the goroutine is gone; record the count and ensure it doesn't
	// keep growing.
	settled := fa.actions.Load()
	time.Sleep(50 * time.Millisecond)
	if got := fa.actions.Load(); got != settled {
		t.Fatalf("actions kept firing after Stop: %d → %d", settled, got)
	}
}

func TestManager_StartIdempotentPerChat(t *testing.T) {
	fa := &fakeActionAdapter{}
	reg := channels.NewRegistry()
	_ = reg.Register(fa)
	m := New(reg, quiet())
	defer m.Stop("telegram", "1")

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m.Start(context.Background(), "telegram", "1") }()
	}
	wg.Wait()

	m.mu.Lock()
	n := len(m.active)
	m.mu.Unlock()
	if n != 1 {
		t.Fatalf("expected a single active typing loop for the chat, got %d", n)
	}
}

func TestManager_NoActionSenderIsNoop(t *testing.T) {
	reg := channels.NewRegistry()
	_ = reg.Register(plainAdapter{})
	m := New(reg, quiet())

	m.Start(context.Background(), "plain", "1") // must not register a loop
	m.mu.Lock()
	n := len(m.active)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("expected no active loop for a non-ActionSender channel, got %d", n)
	}
}

func TestManager_NilRegistryNoop(t *testing.T) {
	m := New(nil, quiet())
	m.Start(context.Background(), "telegram", "1") // must not panic
	m.Stop("telegram", "1")
}

func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
