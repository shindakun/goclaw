// Package typing shows a "typing…"-style chat action while a reply is being
// produced, refreshing it until the reply is delivered. A Telegram chat action
// auto-expires after ~5s, so a per-chat goroutine re-sends it on an interval
// from the moment a message is accepted (router) until the reply goes out
// (delivery).
//
// It's a best-effort UX nicety: failures to send the action are ignored, and a
// channel whose adapter doesn't implement channels.ActionSender simply shows no
// indicator.
package typing

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/shindakun/goclaw/internal/channels"
)

// refreshInterval is how often the action is re-sent. Telegram's action lasts
// ~5s; 4s keeps it continuously visible with a little margin.
const refreshInterval = 4 * time.Second

// Manager starts and stops per-chat typing indicators. Safe for concurrent use:
// the router calls Start and the delivery loop calls Stop from different loops.
type Manager struct {
	registry *channels.Registry
	log      *slog.Logger

	mu     sync.Mutex
	active map[string]context.CancelFunc // key: channel + "\x00" + chatID
}

// New constructs a Manager over the channel registry. A nil registry disables
// it (Start/Stop become no-ops), so callers needn't special-case its absence.
func New(registry *channels.Registry, log *slog.Logger) *Manager {
	return &Manager{
		registry: registry,
		log:      log,
		active:   make(map[string]context.CancelFunc),
	}
}

func key(channel, chatID string) string { return channel + "\x00" + chatID }

// Start begins showing the typing indicator for a chat and keeps refreshing it
// until Stop (or the parent ctx is cancelled). Calling Start again for the same
// chat is a no-op while one is already active. If the channel's adapter doesn't
// support actions, Start does nothing.
func (m *Manager) Start(ctx context.Context, channel, chatID string) {
	if m == nil || m.registry == nil {
		return
	}
	adapter, ok := m.registry.Get(channel)
	if !ok {
		return
	}
	sender, ok := adapter.(channels.ActionSender)
	if !ok {
		return // channel can't show actions — no indicator
	}

	k := key(channel, chatID)
	m.mu.Lock()
	if _, exists := m.active[k]; exists {
		m.mu.Unlock()
		return // already typing for this chat
	}
	loopCtx, cancel := context.WithCancel(ctx)
	m.active[k] = cancel
	m.mu.Unlock()

	go m.loop(loopCtx, sender, chatID)
}

// Stop ends the typing indicator for a chat (called after a reply is sent).
func (m *Manager) Stop(channel, chatID string) {
	if m == nil {
		return
	}
	k := key(channel, chatID)
	m.mu.Lock()
	cancel := m.active[k]
	delete(m.active, k)
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// loop sends the action immediately, then on every tick until cancelled.
func (m *Manager) loop(ctx context.Context, sender channels.ActionSender, chatID string) {
	send := func() {
		if err := sender.SendAction(ctx, chatID, "typing"); err != nil && ctx.Err() == nil {
			m.log.Debug("typing action failed", "chat", chatID, "err", err)
		}
	}
	send()
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			send()
		}
	}
}
