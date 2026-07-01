package main

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── logBuffer ───────────────────────────────────────────────────────────────

// logBuffer is a thread-safe ring of the most recent log lines. It implements
// io.Writer so the standard logger can target it (log.SetOutput), splitting the
// byte stream into lines and keeping at most max of them.
type logBuffer struct {
	mu      sync.Mutex
	lines   []string
	partial []byte
	max     int
}

func newLogBuffer(max int) *logBuffer { return &logBuffer{max: max} }

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.partial = append(b.partial, p...)
	for {
		i := bytes.IndexByte(b.partial, '\n')
		if i < 0 {
			break
		}
		b.lines = append(b.lines, string(b.partial[:i]))
		b.partial = b.partial[i+1:]
		if len(b.lines) > b.max {
			b.lines = b.lines[len(b.lines)-b.max:]
		}
	}
	return len(p), nil
}

// tail returns the last n stored lines (fewer if not yet that many).
func (b *logBuffer) tail(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || len(b.lines) == 0 {
		return nil
	}
	if n > len(b.lines) {
		n = len(b.lines)
	}
	out := make([]string, n)
	copy(out, b.lines[len(b.lines)-n:])
	return out
}

// ── styles ──────────────────────────────────────────────────────────────────

var (
	colGreen = lipgloss.Color("#2ecc71")
	colAmber = lipgloss.Color("#f1c40f")
	colRed   = lipgloss.Color("#e74c3c")
	colGrey  = lipgloss.Color("#7f8c8d")
	colDim   = lipgloss.Color("#5c6370")
	colTitle = lipgloss.Color("#61afef")

	titleStyle  = lipgloss.NewStyle().Foreground(colTitle).Bold(true)
	dimStyle    = lipgloss.NewStyle().Foreground(colDim)
	boxStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colDim).Padding(0, 1)
	headerStyle = lipgloss.NewStyle().Foreground(colDim).Bold(true)
)

// ── model ───────────────────────────────────────────────────────────────────

type tickMsg time.Time

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// uiModel is the dashboard: a live table of providers (top) over a log pane
// (bottom half). It polls the rotator and log buffer once a second.
type uiModel struct {
	rot           *Rotator
	logs          *logBuffer
	addr          string
	width, height int
}

func newUIModel(rot *Rotator, logs *logBuffer, addr string) uiModel {
	return uiModel{rot: rot, logs: logs, addr: addr}
}

func (m uiModel) Init() tea.Cmd { return tick() }

func (m uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "t":
			// Probe every configured model with a hello-world prompt; results are
			// logged to the pane and fold back into the table. Runs in the
			// background so the dashboard stays responsive.
			go runTest(m.rot)
		}
	case tickMsg:
		return m, tick()
	}
	return m, nil
}

func (m uiModel) View() string {
	if m.width == 0 || m.height == 0 {
		return "starting chicco dashboard…"
	}
	logH := m.height / 2
	topH := m.height - logH
	top := m.renderModels(m.width, topH)
	bottom := m.renderLogs(m.width, logH)
	return lipgloss.JoinVertical(lipgloss.Left, top, bottom)
}

// renderModels draws the provider table inside a box of exactly w×h.
func (m uiModel) renderModels(w, h int) string {
	innerW := w - 4 // border (2) + horizontal padding (2)
	innerH := h - 2 // border (2)
	stats := m.rot.Snapshot()

	lines := []string{
		titleStyle.Render("chicco") + dimStyle.Render(fmt.Sprintf(" · %s · %d providers", m.addr, len(stats))),
		headerStyle.Render(modelRow("", "STATUS", "MODEL", "USED / QUOTA", "REQS", "", innerW, true)),
	}
	maxRows := innerH - len(lines) - 1 // reserve the last inner line for the legend
	for i, s := range stats {
		if i >= maxRows {
			break
		}
		lines = append(lines, renderProviderRow(s, innerW))
	}
	lines = append(lines, legendLine())
	return boxStyle.Width(innerW).Height(innerH).Render(strings.Join(lines, "\n"))
}

// legendLine is the colour key for the status dots plus the quit hint.
func legendLine() string {
	g := func(c lipgloss.Color, glyph, label string) string {
		return lipgloss.NewStyle().Foreground(c).Render(glyph) + dimStyle.Render(" "+label)
	}
	sep := dimStyle.Render("   ")
	return g(colGreen, "●", "ready") + sep + g(colAmber, "◐", "cooldown / limit") + sep +
		g(colGrey, "●", "bad key / down") + sep + g(colGrey, "○", "checking") + sep + dimStyle.Render("t test all · q quits")
}

// renderLogs draws the tail of the log buffer inside a box of exactly w×h.
func (m uiModel) renderLogs(w, h int) string {
	innerW := w - 4
	innerH := h - 2
	var b strings.Builder
	b.WriteString(titleStyle.Render("logs"))
	b.WriteString("\n")
	lines := m.logs.tail(innerH - 1) // minus the title line
	for i, ln := range lines {
		b.WriteString(dimStyle.Render(truncate(ln, innerW)))
		if i < len(lines)-1 {
			b.WriteString("\n")
		}
	}
	return boxStyle.Width(innerW).Height(innerH).Render(b.String())
}

// renderProviderRow formats one provider's stat line: status dot, name, token
// usage, request count, and a coloured usage bar — or a status note when the
// provider is greyed (bad key / unreachable / not yet probed) or in cooldown.
func renderProviderRow(s ProviderStat, width int) string {
	grey := lipgloss.NewStyle().Foreground(colGrey)
	green := lipgloss.NewStyle().Foreground(colGreen)
	amber := lipgloss.NewStyle().Foreground(colAmber)

	// Governing quota: tokens take priority, then request count.
	tokenQuota := s.QuotaTokens > 0
	reqQuota := !tokenQuota && s.QuotaRequests > 0

	var usage string
	switch {
	case tokenQuota:
		usage = fmt.Sprintf("%s / %s", fmtTok(s.UsedTokens), fmtTok(s.QuotaTokens))
	case reqQuota:
		usage = fmt.Sprintf("%d / %d req", s.Requests, s.QuotaRequests)
	default:
		usage = fmt.Sprintf("%s / —", fmtTok(s.UsedTokens))
	}

	// Reserve room for a "  cd 47s" suffix when in cooldown so the bar still fits.
	reserve := 0
	if s.CooldownLeft > 0 {
		reserve = 12
	}
	barTail := func(pct float64) string {
		// Leave slack beyond the name(20)+model(24)+usage(18)+reqs(9) cells, the
		// " 100%" suffix, and any cooldown note so the bar never wraps.
		barW := width - 20 - 24 - 18 - 9 - 8 - reserve
		if barW < 6 {
			barW = 6
		}
		if barW > 64 {
			barW = 64
		}
		return renderBar(pct, barW) + fmt.Sprintf(" %3.0f%%", pct*100)
	}

	// The usage bar (or no-quota note) is shown regardless of cooldown.
	var bar string
	switch {
	case tokenQuota:
		bar = barTail(float64(s.UsedTokens) / float64(s.QuotaTokens))
	case reqQuota:
		bar = barTail(float64(s.Requests) / float64(s.QuotaRequests))
	default:
		bar = dimStyle.Render("(no quota)")
	}

	// Dot + trailing column by precedence: a dead/unknown key greys the row (no
	// bar — we can't reach it); a cooldown keeps the bar and appends the timer;
	// otherwise it's a healthy green row with its bar.
	var dot, tail string
	switch {
	case s.Health == HealthAuth:
		dot, tail = grey.Render("●"), grey.Render("auth failed — check API key")
	case s.Health == HealthDown:
		dot, tail = grey.Render("●"), grey.Render("unreachable")
	case s.Health == HealthUnknown:
		dot, tail = grey.Render("○"), grey.Render("checking…")
	case s.CooldownLeft > 0 && s.CooldownKind == "limit":
		// Window/usage limit hit — show when it reopens, not a generic cooldown.
		dot = amber.Render("◐")
		tail = amber.Render("limit · resets " + fmtReset(s.CooldownLeft))
	case s.CooldownLeft > 0:
		dot = amber.Render("◐")
		tail = bar + amber.Render("  cd "+s.CooldownLeft.Round(time.Second).String())
	default:
		dot, tail = green.Render("●"), bar
	}
	return modelRow(dot, s.Name, joinModels(s.Models), usage, fmt.Sprintf("req %d", s.Requests), tail, width, false)
}

// joinModels renders the model(s) behind a provider for the dashboard: the single
// model name, or a comma-joined list (truncated by the cell) for a multi-model
// provider that round-robins them.
func joinModels(models []string) string {
	if len(models) == 0 {
		return "—"
	}
	return strings.Join(models, ", ")
}

// modelRow lays out the columns to fixed widths so the table aligns. name, model,
// usage and reqs are plain text (truncated + padded — never wrapped, which would
// break the row); the dot and tail carry their own ANSI and are placed as-is.
func modelRow(dot, name, model, usage, reqs, tail string, width int, header bool) string {
	cell := func(s string, w int) string {
		s = truncate(s, w-1) // w-1 so a truncated cell keeps a 1-space column gap
		if pad := w - len([]rune(s)); pad > 0 {
			s += strings.Repeat(" ", pad)
		}
		return s
	}
	if header {
		return cell("  "+name, 20) + cell(model, 24) + cell(usage, 18) + cell(reqs, 9) + "USAGE"
	}
	// dot (styled, 1 col) + space fills the first 2 cols; name fills the next 18.
	return dot + " " + cell(name, 18) + dimStyle.Render(cell(model, 24)) + cell(usage, 18) + cell(reqs, 9) + tail
}

// ── helpers ─────────────────────────────────────────────────────────────────

func renderBar(pct float64, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	if width < 4 {
		width = 4
	}
	filled := int(pct*float64(width) + 0.5)
	if filled > width {
		filled = width
	}
	col := colGreen
	switch {
	case pct >= 0.85:
		col = colRed
	case pct >= 0.6:
		col = colAmber
	}
	full := lipgloss.NewStyle().Foreground(col).Render(strings.Repeat("█", filled))
	empty := dimStyle.Render(strings.Repeat("░", width-filled))
	return full + empty
}

// fmtReset renders when a usage window reopens (now + remaining): a clock time
// for a same-day reset, otherwise a dated time.
func fmtReset(left time.Duration) string {
	t := time.Now().Add(left)
	if left < 12*time.Hour {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

func fmtTok(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w <= 1 {
		return string(r[:w])
	}
	return string(r[:w-1]) + "…"
}
