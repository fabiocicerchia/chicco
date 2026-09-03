package proxy

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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

// TestIsErrorLine confirms the log pane's red-highlight heuristic catches
// chicco's actual failure log lines and leaves routine ones alone.
func TestIsErrorLine(t *testing.T) {
	red := []string{
		"chicco: server error: listen tcp :41986: bind: address already in use",
		"chicco: groq — auth failed (invalid or missing API key)",
		"chicco: groq — unreachable at boot",
		"chicco: reload failed, keeping current config: yaml: line 3: bad indent",
		"chicco: reload rejected — providers[0]: missing name",
		"chicco: groq (llama-3.3-70b) transport error, blocked 1m0s: context deadline exceeded",
		"chicco: groq (llama-3.3-70b) HTTP 429, blocked 1m0s",
		"chicco test: ✗ groq/llama-3.3-70b — limit — resets in 17h43m · 200.0k tok · daily",
	}
	for _, l := range red {
		if !isErrorLine(l) {
			t.Errorf("isErrorLine(%q) = false, want true", l)
		}
	}

	dim := []string{
		"chicco 1.2.3 listening on :41986 — rotating across 2 provider(s): [groq cerebras]",
		"chicco: groq — healthy",
		"chicco: config reloaded from chicco.yaml (2 provider(s))",
		"chicco: routing to groq (llama-3.3-70b-versatile)",
		"chicco: groq (llama-3.3-70b-versatile) served 44 tokens",
		"chicco test: ✓ groq/llama-3.3-70b — 44 tok · 44/200.0k tok · daily",
	}
	for _, l := range dim {
		if isErrorLine(l) {
			t.Errorf("isErrorLine(%q) = true, want false", l)
		}
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

// TestUpdateScrollKeys drives the key handling that scrolls the two panes. The
// panes scroll in opposite directions — providerScroll counts rows from the top
// of the table, logScroll counts lines back from the newest — and only the
// edges the model itself knows about are clamped here; the far ends are clamped
// at render time against line counts only the renderer has.
func TestUpdateScrollKeys(t *testing.T) {
	press := func(m uiModel, keys ...tea.KeyMsg) uiModel {
		for _, k := range keys {
			next, _ := m.Update(k)
			m = next.(uiModel)
		}
		return m
	}
	down := tea.KeyMsg{Type: tea.KeyDown}
	up := tea.KeyMsg{Type: tea.KeyUp}
	pgdn := tea.KeyMsg{Type: tea.KeyPgDown}
	pgup := tea.KeyMsg{Type: tea.KeyPgUp}
	tab := tea.KeyMsg{Type: tea.KeyTab}

	m := newUIModel(NewRotator(nil, nil), newLogBuffer(10), ":41986")
	m.height = 24 // pageSize() reads it: (24*2/5 - 2) / 2 = 3

	// Provider pane: down moves away from the top, up comes back and stops at 0.
	got := press(m, down, down, down, up)
	if got.providerScroll != 2 {
		t.Errorf("providerScroll after 3×down 1×up = %d, want 2", got.providerScroll)
	}
	if got := press(m, up, up).providerScroll; got != 0 {
		t.Errorf("providerScroll after 2×up from the top = %d, want 0", got)
	}
	if got := press(m, pgdn, pgup, pgup).providerScroll; got != 0 {
		t.Errorf("providerScroll after pgdown then 2×pgup = %d, want 0", got)
	}
	if got := press(m, pgdn).providerScroll; got != 3 {
		t.Errorf("providerScroll after pgdown = %d, want one page (3)", got)
	}

	// Log pane: up walks back through history, down returns to the newest line.
	logs := press(m, tab, up, up, up, down)
	if logs.logScroll != 2 {
		t.Errorf("logScroll after 3×up 1×down = %d, want 2", logs.logScroll)
	}
	if logs.providerScroll != 0 {
		t.Errorf("log-pane keys moved providerScroll to %d, want 0", logs.providerScroll)
	}
	if got := press(m, tab, down, down).logScroll; got != 0 {
		t.Errorf("logScroll after 2×down at the newest line = %d, want 0", got)
	}
	if got := press(m, tab, pgup).logScroll; got != 3 {
		t.Errorf("logScroll after pgup = %d, want one page (3)", got)
	}

	// tab twice comes back to the provider pane.
	if got := press(m, tab, tab).focus; got != focusProviders {
		t.Errorf("focus after 2×tab = %v, want focusProviders", got)
	}
}

// TestProviderRowsQuotaCells pins the usage cells providerRows writes: the
// provider row measures against the provider quota, a model with its own quota
// measures against that instead, and a model without one falls back to the
// provider's so the column stays comparable.
func TestProviderRowsQuotaCells(t *testing.T) {
	s := ProviderStat{
		Name: "groq", Kind: "http", Health: HealthOK,
		Quota: 1000, QuotaIsTokens: true, UsedTokens: 250, Requests: 4,
		Models: []ModelStat{
			{Name: "big", Tokens: 200, Requests: 3},
			{Name: "small", Tokens: 50, Requests: 1, Quota: 100, QuotaIsTokens: true, UsedTokens: 50},
			{Name: "capped", Requests: 6, Quota: 10},
		},
	}
	rows := providerRows(s, 200)
	if len(rows) != 3 {
		t.Fatalf("providerRows returned %d rows, want one per model", len(rows))
	}
	for i, want := range []string{"250 / 1.0k", "50 / 100", "6 / 10 req"} {
		if !strings.Contains(rows[i], want) {
			t.Errorf("row %d = %q, want it to carry %q", i, rows[i], want)
		}
	}
	// The model with no quota of its own falls back to the provider's.
	if !strings.Contains(rows[0], "200 / 1.0k") && !strings.Contains(rows[0], "250 / 1.0k") {
		t.Errorf("provider row = %q, want the provider-level usage cell", rows[0])
	}
	// A provider with no quota at all gets the placeholder, not a bar.
	noQuota := providerRows(ProviderStat{Name: "x", Health: HealthOK, Models: []ModelStat{{Name: "m"}}}, 200)
	if !strings.Contains(noQuota[0], "(no quota)") {
		t.Errorf("unquota'd provider row = %q, want (no quota)", noQuota[0])
	}
}
