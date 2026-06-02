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

// Get returns the adapter for a channel name, or false if absent.
func (r *Registry) Get(name string) (ChannelAdapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[name]
	return a, ok
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
