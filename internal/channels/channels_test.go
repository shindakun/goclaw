package channels

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// fakeAdapter is a minimal ChannelAdapter for registry tests.
type fakeAdapter struct {
	name string
	in   chan InboundMsg
	mu   sync.Mutex
	sent []OutboundMsg
}

func newFake(name string) *fakeAdapter {
	return &fakeAdapter{name: name, in: make(chan InboundMsg, 1)}
}

func (f *fakeAdapter) Name() string { return f.name }
func (f *fakeAdapter) Start(ctx context.Context) (<-chan InboundMsg, error) {
	return f.in, nil
}
func (f *fakeAdapter) Send(ctx context.Context, out OutboundMsg) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, out)
	return nil
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	a := newFake("telegram")
	if err := r.Register(a); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Duplicate name is rejected.
	if err := r.Register(newFake("telegram")); err == nil {
		t.Fatal("expected error registering duplicate name")
	}
	got, ok := r.Get("telegram")
	if !ok || got != a {
		t.Fatalf("Get returned %v, %v", got, ok)
	}
	if _, ok := r.Get("absent"); ok {
		t.Fatal("Get on absent channel should be false")
	}
	if len(r.All()) != 1 {
		t.Fatalf("All() = %d, want 1", len(r.All()))
	}
}

func TestRegistry_Unregister(t *testing.T) {
	r := NewRegistry()
	a, b := newFake("telegram"), newFake("discord")
	_ = r.Register(a)
	_ = r.Register(b)

	// Unregistering an absent name reports false and changes nothing.
	if r.Unregister("absent") {
		t.Fatal("Unregister(absent) = true, want false")
	}
	if len(r.All()) != 2 {
		t.Fatalf("All() = %d after no-op unregister, want 2", len(r.All()))
	}

	// Unregistering a present name reports true and makes it un-Gettable...
	if !r.Unregister("telegram") {
		t.Fatal("Unregister(telegram) = false, want true")
	}
	if _, ok := r.Get("telegram"); ok {
		t.Fatal("telegram still Gettable after Unregister")
	}
	// ...while leaving the other adapter intact.
	if _, ok := r.Get("discord"); !ok {
		t.Fatal("Unregister(telegram) wrongly removed discord")
	}
	if len(r.All()) != 1 {
		t.Fatalf("All() = %d after unregister, want 1", len(r.All()))
	}

	// The freed name can be registered again (hot-reload reinstall path).
	if err := r.Register(newFake("telegram")); err != nil {
		t.Fatalf("re-Register after Unregister: %v", err)
	}

	// A second unregister of the same (now re-registered) name still works, and a
	// third reports false (idempotent).
	if !r.Unregister("telegram") {
		t.Fatal("second Unregister(telegram) = false, want true")
	}
	if r.Unregister("telegram") {
		t.Fatal("third Unregister(telegram) = true, want false")
	}
}

func TestRegistry_Send(t *testing.T) {
	r := NewRegistry()
	a := newFake("discord")
	_ = r.Register(a)

	if err := r.Send(context.Background(), OutboundMsg{Channel: "discord", Text: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(a.sent) != 1 || a.sent[0].Text != "hi" {
		t.Fatalf("adapter did not receive send: %+v", a.sent)
	}
	// Unknown channel errors.
	if err := r.Send(context.Background(), OutboundMsg{Channel: "nope"}); err == nil {
		t.Fatal("expected error sending to unregistered channel")
	}
}

// StartAll fans every adapter's inbound into one channel, and closes it on
// context cancel once goroutines drain.
func TestRegistry_StartAllFanIn(t *testing.T) {
	r := NewRegistry()
	a, b := newFake("telegram"), newFake("discord")
	_ = r.Register(a)
	_ = r.Register(b)

	ctx, cancel := context.WithCancel(context.Background())
	fan, err := r.StartAll(ctx)
	if err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	a.in <- InboundMsg{Channel: "telegram", Text: "from-a"}
	b.in <- InboundMsg{Channel: "discord", Text: "from-b"}

	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		msg := <-fan
		got[msg.Channel] = true
	}
	if !got["telegram"] || !got["discord"] {
		t.Fatalf("fan-in missing a channel: %v", got)
	}

	// Cancel and confirm the fan-in channel closes.
	cancel()
	for range fan { //nolint:revive // drain until closed
	}
}

func TestSplitMessage(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want int // number of chunks
	}{
		{"empty yields one chunk", "", 2000, 1},
		{"short fits in one", "hello", 2000, 1},
		{"exactly at limit is one", strings.Repeat("a", 2000), 2000, 1},
		{"one over splits to two", strings.Repeat("a", 2001), 2000, 2},
		{"four times over", strings.Repeat("a", 8000), 2000, 4},
		{"telegram limit", strings.Repeat("a", 4097), 4096, 2},
		{"nonpositive max is one chunk", "abc", 0, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SplitMessage(c.in, c.max)
			if len(got) != c.want {
				t.Fatalf("chunks = %d, want %d", len(got), c.want)
			}
			if c.max > 0 {
				for i, ch := range got {
					if len([]rune(ch)) > c.max {
						t.Fatalf("chunk %d has %d runes, exceeds max %d", i, len([]rune(ch)), c.max)
					}
				}
			}
			if strings.Join(got, "") != c.in {
				t.Fatalf("rejoined chunks != original input")
			}
		})
	}
}

// A multi-byte rune must never be cut in half by the splitter.
func TestSplitMessage_RuneBoundary(t *testing.T) {
	// 3 two-byte runes, max 2 runes -> chunks of "éé" and "é", never a broken byte.
	in := strings.Repeat("é", 3)
	got := SplitMessage(in, 2)
	if len(got) != 2 {
		t.Fatalf("chunks = %d, want 2", len(got))
	}
	if got[0] != "éé" || got[1] != "é" {
		t.Fatalf("bad split: %q", got)
	}
}
