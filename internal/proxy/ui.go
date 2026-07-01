package proxy

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

// allLines returns a copy of all stored lines, oldest first.
func (b *logBuffer) allLines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.lines))
	copy(out, b.lines)
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

// focusPane identifies which panel currently has keyboard focus.
type focusPane int

const (
	focusProviders focusPane = iota
	focusLogs
)

// uiModel is the dashboard: a live table of providers (top) over a log pane
// (bottom). It polls the rotator and log buffer once a second.
type uiModel struct {
	rot            *Rotator
	logs           *logBuffer
	addr           string
	width, height  int
	focus          focusPane
	providerScroll int // rows scrolled down in the provider table (0 = top)
	logScroll      int // rows scrolled up from the bottom in the log pane (0 = bottom)
}

func newUIModel(rot *Rotator, logs *logBuffer, addr string) uiModel {
	return uiModel{rot: rot, logs: logs, addr: addr, focus: focusProviders}
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
		case "tab":
			if m.focus == focusProviders {
				m.focus = focusLogs
			} else {
				m.focus = focusProviders
			}
		case "up", "k":
			if m.focus == focusProviders {
				if m.providerScroll > 0 {
					m.providerScroll--
				}
			} else {
				m.logScroll++
			}
		case "down", "j":
			if m.focus == focusProviders {
				m.providerScroll++
			} else {
				if m.logScroll > 0 {
					m.logScroll--
				}
			}
		case "pgup":
			page := m.pageSize()
			if m.focus == focusProviders {
				m.providerScroll -= page
				if m.providerScroll < 0 {
					m.providerScroll = 0
				}
			} else {
				m.logScroll += page
			}
		case "pgdown":
			page := m.pageSize()
			if m.focus == focusProviders {
				m.providerScroll += page
			} else {
				m.logScroll -= page
				if m.logScroll < 0 {
					m.logScroll = 0
				}
			}
		}
	case tickMsg:
		return m, tick()
	}
	return m, nil
}

// pageSize returns how many lines to jump on pgup/pgdown — half the log pane height.
func (m uiModel) pageSize() int {
	logH := m.height * 2 / 5
	p := (logH - 2) / 2
	if p < 1 {
		p = 1
	}
	return p
}

func (m uiModel) View() string {
	if m.width == 0 || m.height == 0 {
		return "starting chicco dashboard…"
	}
	logH := m.height * 2 / 5
	if logH < 4 {
		logH = 4
	}
	topH := m.height - logH
	top := m.renderModels(m.width, topH, m.providerScroll, m.focus == focusProviders)
	bottom := m.renderLogs(m.width, logH, m.logScroll, m.focus == focusLogs)
	return lipgloss.JoinVertical(lipgloss.Left, top, bottom)
}

// renderModels draws the provider table inside a box of exactly w×h.
// scroll is how many provider rows have been scrolled past the top (0 = top).
// focused controls whether the border is highlighted.
func (m uiModel) renderModels(w, h int, scroll int, focused bool) string {
	innerW := w - 4 // border (2) + horizontal padding (2)
	innerH := h - 2 // border (2)
	stats := m.rot.Snapshot()

	header := []string{
		titleStyle.Render("chicco") + dimStyle.Render(fmt.Sprintf(" · %s · %d providers", m.addr, len(stats))),
		headerStyle.Render(modelRow("", "STATUS", "KIND", "MODEL", "USED / QUOTA", "REQS", "", innerW, true)),
	}
	// Collect all provider rows, then apply scroll.
	maxRows := innerH - len(header) - 1 // reserve last line for legend
	var allRows []string
	for _, s := range stats {
		allRows = append(allRows, providerRows(s, innerW)...)
	}
	// Clamp scroll so we never scroll past the last screenful.
	maxScroll := len(allRows) - maxRows
	if maxScroll < 0 {
		maxScroll = 0
	}
	if scroll > maxScroll {
		scroll = maxScroll
	}
	visible := allRows
	if scroll > 0 && scroll < len(allRows) {
		visible = allRows[scroll:]
	}
	if len(visible) > maxRows {
		visible = visible[:maxRows]
	}

	lines := append(header, visible...)
	lines = append(lines, legendLine())

	style := boxStyle.Width(innerW).Height(innerH)
	if focused {
		style = style.BorderForeground(colTitle)
	}
	return style.Render(strings.Join(lines, "\n"))
}

// legendLine is the colour key for the status dots plus key hints.
func legendLine() string {
	g := func(c lipgloss.Color, glyph, label string) string {
		return lipgloss.NewStyle().Foreground(c).Render(glyph) + dimStyle.Render(" "+label)
	}
	sep := dimStyle.Render("   ")
	return g(colGreen, "●", "ready") + sep + g(colAmber, "◐", "cooldown / limit") + sep +
		g(colGrey, "●", "bad key / down") + sep + g(colGrey, "○", "checking") + sep +
		dimStyle.Render("tab focus · ↑↓/pgup/pgdn scroll · t test · q quit")
}

// renderLogs draws the log buffer inside a box of exactly w×h.
// scroll is how many lines from the bottom have been scrolled past (0 = bottom/latest).
// focused controls whether the border is highlighted.
func (m uiModel) renderLogs(w, h int, scroll int, focused bool) string {
	innerW := w - 4
	innerH := h - 2
	capacity := innerH - 1 // minus the title line

	all := m.logs.allLines()

	// Clamp scroll so we can't scroll past the oldest line.
	maxScroll := len(all) - capacity
	if maxScroll < 0 {
		maxScroll = 0
	}
	if scroll > maxScroll {
		scroll = maxScroll
	}

	// Select the window: scroll=0 shows the newest lines, scroll=N shows older.
	var window []string
	if len(all) > 0 {
		end := len(all) - scroll
		if end < 0 {
			end = 0
		}
		start := end - capacity
		if start < 0 {
			start = 0
		}
		window = all[start:end]
	}

	var b strings.Builder
	b.WriteString(titleStyle.Render("logs"))
	b.WriteString("\n")
	for i, ln := range window {
		b.WriteString(dimStyle.Render(truncate(ln, innerW)))
		if i < len(window)-1 {
			b.WriteString("\n")
		}
	}

	style := boxStyle.Width(innerW).Height(innerH)
	if focused {
		style = style.BorderForeground(colTitle)
	}
	return style.Render(b.String())
}

// providerRows formats a provider's stat as one row per model. The first row
// carries the provider name, status dot, provider-level usage figures and bar;
// each subsequent model row shows the model name and its own token/request counts
// and bar (scaled against the same provider quota so you can compare).
func providerRows(s ProviderStat, width int) []string {
	grey := lipgloss.NewStyle().Foreground(colGrey)
	green := lipgloss.NewStyle().Foreground(colGreen)
	amber := lipgloss.NewStyle().Foreground(colAmber)

	// Governing quota: tokens take priority, then request count.
	tokenQuota := s.QuotaIsTokens && s.Quota > 0
	reqQuota := !s.QuotaIsTokens && s.Quota > 0

	providerUsage := func(tokens int64, reqs int) string {
		switch {
		case tokenQuota:
			return fmt.Sprintf("%s / %s", fmtTok(tokens), fmtTok(s.Quota))
		case reqQuota:
			return fmt.Sprintf("%d / %d req", reqs, int(s.Quota))
		default:
			return fmt.Sprintf("%s / —", fmtTok(tokens))
		}
	}

	// Reserve room for a "  cd 47s" suffix when in cooldown so the bar still fits.
	reserve := 0
	if s.CooldownLeft > 0 {
		reserve = 12
	}
	barTail := func(pct float64) string {
		barW := width - 20 - 5 - 24 - 18 - 9 - 8 - reserve
		if barW < 6 {
			barW = 6
		}
		if barW > 64 {
			barW = 64
		}
		return renderBar(pct, barW) + fmt.Sprintf(" %3.0f%%", pct*100)
	}

	providerBar := func(tokens int64, reqs int) string {
		switch {
		case tokenQuota:
			return barTail(float64(tokens) / float64(s.Quota))
		case reqQuota:
			return barTail(float64(reqs) / float64(s.Quota))
		default:
			return dimStyle.Render("(no quota)")
		}
	}

	var dot, tail string
	switch {
	case s.Health == HealthAuth:
		dot, tail = grey.Render("●"), grey.Render("auth failed — check API key")
	case s.Health == HealthDown:
		dot, tail = grey.Render("●"), grey.Render("unreachable")
	case s.Health == HealthUnknown:
		dot, tail = grey.Render("○"), grey.Render("checking…")
	case s.CooldownLeft > 0 && s.CooldownKind == "limit":
		dot = amber.Render("◐")
		tail = amber.Render("limit · resets " + fmtReset(s.CooldownLeft))
	case s.CooldownLeft > 0:
		dot = amber.Render("◐")
		tail = providerBar(s.UsedTokens, s.Requests) + amber.Render("  cd "+s.CooldownLeft.Round(time.Second).String())
	default:
		dot = green.Render("●")
		tail = providerBar(s.UsedTokens, s.Requests)
	}

	models := s.Models
	if len(models) == 0 {
		models = []ModelStat{{Name: "—"}}
	}

	rows := make([]string, 0, len(models))
	for i, ms := range models {
		if i == 0 {
			// First row: provider name + dot + kind + provider-level usage.
			rows = append(rows, modelRow(dot, s.Name, s.Kind, ms.Name,
				providerUsage(s.UsedTokens, s.Requests),
				fmt.Sprintf("req %d", s.Requests),
				tail, width, false))
		} else {
			// Continuation rows: blank name/dot/kind, per-model usage and bar.
			modelUsage := providerUsage(ms.Tokens, ms.Requests)
			modelTail := providerBar(ms.Tokens, ms.Requests)
			rows = append(rows, modelRow(" ", "", "", ms.Name,
				modelUsage,
				fmt.Sprintf("req %d", ms.Requests),
				modelTail, width, false))
		}
	}
	return rows
}

// modelRow lays out the columns to fixed widths so the table aligns. name, kind,
// model, usage and reqs are plain text (truncated + padded — never wrapped, which
// would break the row); the dot and tail carry their own ANSI and are placed as-is.
func modelRow(dot, name, kind, model, usage, reqs, tail string, width int, header bool) string {
	cell := func(s string, w int) string {
		s = truncate(s, w-1) // w-1 so a truncated cell keeps a 1-space column gap
		if pad := w - len([]rune(s)); pad > 0 {
			s += strings.Repeat(" ", pad)
		}
		return s
	}
	if header {
		return cell("  "+name, 20) + cell(kind, 5) + cell(model, 24) + cell(usage, 18) + cell(reqs, 9) + "USAGE"
	}
	// dot (styled, 1 col) + space fills the first 2 cols; name fills the next 18.
	return dot + " " + cell(name, 18) + dimStyle.Render(cell(kind, 5)) + dimStyle.Render(cell(model, 24)) + cell(usage, 18) + cell(reqs, 9) + tail
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
