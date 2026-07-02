package proxy

import (
	"strings"
	"testing"
)

func TestUsageTokens(t *testing.T) {
	cases := []struct {
		line string
		want int64
	}{
		{`data: {"choices":[],"usage":{"total_tokens":1234}}`, 1234},
		{`data: {"usage":{"prompt_tokens":10,"total_tokens":42}}`, 42},
		{`{"id":"x","usage":{"total_tokens":7}}`, 7}, // non-streamed body
		{`data: {"choices":[{"delta":{"content":"hi"}}]}`, 0},
		{"data: [DONE]", 0},
		{": keep-alive", 0},
		{"", 0},
	}
	for _, c := range cases {
		if got := usageTokens([]byte(c.line)); got != c.want {
			t.Errorf("usageTokens(%q) = %d, want %d", c.line, got, c.want)
		}
	}
}

func TestLogBufferRingAndSplit(t *testing.T) {
	b := newLogBuffer(3)
	b.Write([]byte("one\ntwo\n"))
	b.Write([]byte("par")) // partial line, not yet flushed
	b.Write([]byte("tial\n"))
	b.Write([]byte("four\nfive\n"))

	got := b.tail(10)
	// max=3 keeps only the last three completed lines.
	want := []string{"partial", "four", "five"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("tail = %v, want %v", got, want)
	}
}

func TestRenderBarColorAndFill(t *testing.T) {
	// Empty bar is all light blocks; full bar is all solid blocks.
	if strings.Contains(renderBar(0, 10), "█") {
		t.Error("0%% bar should have no filled blocks")
	}
	if strings.Contains(renderBar(1, 10), "░") {
		t.Error("100%% bar should have no empty blocks")
	}
}

// TestScrollbarColumn checks the thumb tracks scroll position and disappears
// (blank track) once everything fits.
func TestScrollbarColumn(t *testing.T) {
	// Fits entirely: no bar, just blanks.
	for _, c := range scrollbarColumn(5, 10, 0, 10) {
		if c != " " {
			t.Fatalf("fits-entirely column = %q, want blank", c)
		}
	}

	// Overflowing list: thumb at the top when offset is 0...
	top := scrollbarColumn(100, 10, 0, 10)
	if top[0] != "█" || top[len(top)-1] != "│" {
		t.Errorf("top scroll column = %v, want thumb at index 0", top)
	}
	// ...and at the bottom when scrolled all the way to the max offset.
	bottom := scrollbarColumn(100, 10, 90, 10)
	if bottom[len(bottom)-1] != "█" || bottom[0] != "│" {
		t.Errorf("bottom scroll column = %v, want thumb at the last index", bottom)
	}
}

// TestViewNoPanic renders the dashboard at a few sizes to catch layout panics.
func TestViewNoPanic(t *testing.T) {
	rot := NewRotator([]Provider{
		{Name: "groq", APIKey: "k", Models: []string{"llama-3.3-70b"}, Quota: Quota{TPD: 1000}},
		{Name: "nofree", APIKey: "k", Models: []string{"m"}}, // no quota
	}, nil)
	rot.recordUsage("groq", "llama-3.3-70b", 600)
	logs := newLogBuffer(50)
	logs.Write([]byte("hello\nworld\n"))
	m := newUIModel(rot, logs, ":41986")
	for _, sz := range [][2]int{{80, 24}, {40, 10}, {120, 50}} {
		m.width, m.height = sz[0], sz[1]
		out := m.View()
		if out == "" {
			t.Errorf("View() empty at %dx%d", sz[0], sz[1])
		}
		// The model behind a provider is shown in the table (wide enough rows).
		if sz[0] >= 80 && !strings.Contains(out, "llama-3.3-70b") {
			t.Errorf("model name not shown in dashboard at %dx%d", sz[0], sz[1])
		}
	}
}
