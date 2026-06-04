package channels

import (
	"context"
	"fmt"
	"sync"
)

// Registry holds the active adapters, keyed by Name(). The delivery loop looks
// up the target adapter here when draining outbound.db (brief §7.3).
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]ChannelAdapter
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{adapters: make(map[string]ChannelAdapter)}
}

// Register adds an adapter. It returns an error if the name is already taken.
func (r *Registry) Register(a ChannelAdapter) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := a.Name()
	if _, exists := r.adapters[name]; exists {
		return fmt.Errorf("channel %q already registered", name)
	}
	r.adapters[name] = a
	return nil
}

// Unregister removes the adapter for name, returning whether one was present. It
// is safe to call on an absent name (returns false). Hot-removing a channel plugin
// uses this: when the plugin's dir disappears, the host unregisters its relay so the
// router and delivery loop stop dispatching to it. Unregister only drops the
// registry entry; stopping the adapter itself (its Start goroutine, any socket) is
// the caller's responsibility, since the registry does not own those.
func (r *Registry) Unregister(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.adapters[name]; !ok {
		return false
	}
	delete(r.adapters, name)
	return true
}

// Get returns the adapter for a channel name, or false if absent.
func (r *Registry) Get(name string) (ChannelAdapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[name]
	return a, ok
}

// Send dispatches a host-originated message via the target channel's adapter.
// Used for host-sent system messages (e.g. the approval-card flow). Returns an
// error if no adapter is registered for the channel.
func (r *Registry) Send(ctx context.Context, out OutboundMsg) error {
	a, ok := r.Get(out.Channel)
	if !ok {
		return fmt.Errorf("channels: no adapter for %q", out.Channel)
	}
	return a.Send(ctx, out)
}

// All returns the registered adapters in no particular order.
func (r *Registry) All() []ChannelAdapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ChannelAdapter, 0, len(r.adapters))
	for _, a := range r.adapters {
		out = append(out, a)
	}
	return out
}

// StartAll starts every registered adapter and fans their inbound messages into
// a single channel the router can drain. The returned channel is closed when
// ctx is cancelled and every adapter goroutine has stopped (brief §7.3).
func (r *Registry) StartAll(ctx context.Context) (<-chan InboundMsg, error) {
	fanIn := make(chan InboundMsg)
	var wg sync.WaitGroup

	for _, a := range r.All() {
		src, err := a.Start(ctx)
		if err != nil {
			return nil, fmt.Errorf("start channel %q: %w", a.Name(), err)
		}
		wg.Add(1)
		go func(src <-chan InboundMsg) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case msg, ok := <-src:
					if !ok {
						return
					}
					select {
					case fanIn <- msg:
					case <-ctx.Done():
						return
					}
				}
			}
		}(src)
	}

	go func() {
		wg.Wait()
		close(fanIn)
	}()

	return fanIn, nil
}
