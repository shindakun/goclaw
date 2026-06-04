package channels

import (
	"strings"
	"testing"
)

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
