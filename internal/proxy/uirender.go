package proxy

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// uirender.go draws the terminal dashboard: the provider table, the usage bars
// and the log pane. Layout only — it reads a snapshot and returns strings.

// renderModels - Draws the provider table inside a box of exactly w×h. scroll
// is how many provider rows have been scrolled past the top (0 = top). focused
// controls whether the border is highlighted.
func (m uiModel) renderModels(w, h int, scroll int, focused bool) string {
	innerW := w - 4 // border (2) + horizontal padding (2)
	innerH := h - 2 // border (2)
	// boxStyle.Width() counts its own Padding(0, 1) as part of that width, so the
	// real text budget is innerW-2; reserve one more column for the scrollbar.
	contentW := innerW - 3
	stats := m.rot.Snapshot()
	reqToday, tokToday, activeN := m.rot.DailyTotals()

	header := []string{
		titleStyle.Render("chicco") + dimStyle.Render(fmt.Sprintf(
			" · %s · %d providers · today: %d req · %s tokens across %d active",
			m.addr, len(stats), reqToday, fmtTok(tokToday), activeN)),
		headerStyle.Render(modelRow("", "STATUS", "KIND", "MODEL", "USED / QUOTA", "REQS", "", contentW, true)),
	}
	// Collect all provider rows, then apply scroll.
	maxRows := innerH - len(header) - 1 // reserve last line for legend
	var allRows []string
	for _, s := range stats {
		allRows = append(allRows, providerRows(s, contentW)...)
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

	// One scrollbar glyph per row slot, thumb sized/positioned by how much of
	// allRows is currently visible — blank when everything fits.
	track := scrollbarColumn(len(allRows), maxRows, scroll, maxRows)
	lines := append([]string{}, header...)
	for i := 0; i < maxRows; i++ {
		var row string
		if i < len(visible) {
			row = visible[i]
		}
		// Rows without a usage bar (e.g. "checking…", "(no quota)") are shorter
		// than contentW — pad so the scrollbar glyph lands on the last column
		// instead of right after the text.
		if pad := contentW - lipgloss.Width(row); pad > 0 {
			row += strings.Repeat(" ", pad)
		}
		lines = append(lines, row+renderTrack(track[i]))
	}
	lines = append(lines, legendLine())

	style := boxStyle.Width(innerW).Height(innerH)
	if focused {
		style = style.BorderForeground(colTitle)
	}
	return style.Render(strings.Join(lines, "\n"))
}

// legendLine - Is the colour key for the status dots plus key hints.
func legendLine() string {
	g := func(c lipgloss.Color, glyph, label string) string {
		return lipgloss.NewStyle().Foreground(c).Render(glyph) + dimStyle.Render(" "+label)
	}
	sep := dimStyle.Render("   ")
	return g(colGreen, "●", "ready") + sep + g(colAmber, "◐", "cooldown / limit") + sep +
		g(colGrey, "●", "bad key / down") + sep + g(colGrey, "○", "checking") + sep +
		dimStyle.Render("tab focus · ↑↓/pgup/pgdn scroll · t test · q quit")
}

// errKeywords are the substrings (checked case-insensitively) that mark a log
// line as a failure worth flagging in red. Log lines carry no structured
// level, so this is a heuristic over the messages chicco actually logs (see
// health.go, reload.go, proxy.go, test.go) — "error", "fail" (failed/failure),
// "reject" (rejected), "unreachable", and "blocked" (a provider put in
// cooldown) cover every failure path; "✗" catches the `t`-test failure lines.
var errKeywords = []string{"error", "fail", "reject", "unreachable", "blocked", "✗"}

// isErrorLine - Reports whether line should render in red instead of dim grey.
func isErrorLine(line string) bool {
	l := strings.ToLower(line)
	for _, kw := range errKeywords {
		if strings.Contains(l, kw) {
			return true
		}
	}
	return false
}

// renderLogs - Draws the log buffer inside a box of exactly w×h. scroll is how
// many lines from the bottom have been scrolled past (0 = bottom/latest).
// focused controls whether the border is highlighted.
func (m uiModel) renderLogs(w, h int, scroll int, focused bool) string {
	innerW := w - 4
	innerH := h - 2
	// boxStyle.Width() counts its own Padding(0, 1) as part of that width, so the
	// real text budget is innerW-2; reserve one more column for the scrollbar.
	contentW := innerW - 3
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
	// start also doubles as the scrollbar offset: lines scrolled past above it.
	var window []string
	start := 0
	if len(all) > 0 {
		end := len(all) - scroll
		if end < 0 {
			end = 0
		}
		start = end - capacity
		if start < 0 {
			start = 0
		}
		window = all[start:end]
	}

	track := scrollbarColumn(len(all), capacity, start, capacity)
	lines := make([]string, 0, capacity+1)
	lines = append(lines, titleStyle.Render("logs"))
	for i := 0; i < capacity; i++ {
		raw := strings.Repeat(" ", contentW)
		style := dimStyle
		if i < len(window) {
			raw = padRight(truncate(window[i], contentW), contentW)
			if isErrorLine(window[i]) {
				style = errLogStyle
			}
		}
		lines = append(lines, style.Render(raw)+renderTrack(track[i]))
	}

	style := boxStyle.Width(innerW).Height(innerH)
	if focused {
		style = style.BorderForeground(colTitle)
	}
	return style.Render(strings.Join(lines, "\n"))
}

// scrollbarColumn - Renders a trackHeight-tall vertical scrollbar for a list of
// `total` items showing `visible` at a time, with `offset` items scrolled past
// above the current view (0 = at the top). Each element is one column wide: "█"
// for the thumb, "│" for the track, or " " when everything already fits (no bar
// needed).
func scrollbarColumn(total, visible, offset, trackHeight int) []string {
	if trackHeight <= 0 {
		return nil
	}
	col := make([]string, trackHeight)
	if total <= visible || visible <= 0 {
		for i := range col {
			col[i] = " "
		}
		return col
	}
	thumb := trackHeight * visible / total
	if thumb < 1 {
		thumb = 1
	}
	start := 0
	if maxOffset := total - visible; maxOffset > 0 {
		start = offset * (trackHeight - thumb) / maxOffset
	}
	if start > trackHeight-thumb {
		start = trackHeight - thumb
	}
	for i := range col {
		if i >= start && i < start+thumb {
			col[i] = "█"
		} else {
			col[i] = "│"
		}
	}
	return col
}

// modelRow - Lays out the columns to fixed widths so the table aligns. name,
// kind, model, usage and reqs are plain text (truncated + padded — never
// wrapped, which would break the row); the dot and tail carry their own ANSI
// and are placed as-is.
func modelRow(dot, name, kind, model, usage, reqs, tail string, width int, header bool) string {
	cell := func(s string, w int) string {
		return padRight(truncate(s, w-1), w) // w-1 so a truncated cell keeps a 1-space column gap
	}
	if header {
		return cell("  "+name, 20) + cell(kind, 5) + cell(model, 24) + cell(usage, 18) + cell(reqs, 9) + "USAGE"
	}
	// dot (styled, 1 col) + space fills the first 2 cols; name fills the next 18.
	return dot + " " + cell(name, 18) + dimStyle.Render(cell(kind, 5)) + dimStyle.Render(cell(model, 24)) + cell(usage, 18) + cell(reqs, 9) + tail
}

// ── helpers ─────────────────────────────────────────────────────────────────

// renderBar - Draws a usage bar, green until 60% of the quota window, amber to
// 85%, red above it. pct is clamped, because a provider that reports more usage
// than its own quota should show a full bar rather than overflow the row.
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

// fmtReset - Renders when a usage window reopens (now + remaining): a clock
// time for a same-day reset, otherwise a dated time.
func fmtReset(left time.Duration) string {
	t := time.Now().Add(left)
	if left < 12*time.Hour {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// fmtTok - Renders a token count in the narrowest form that still reads:
// 1.2M, 4.5k, or the number itself. The table column is fixed-width.
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

// padRight - Pads s with spaces to width w (rune-aware); a no-op if s already
// fills or exceeds w.
func padRight(s string, w int) string {
	if pad := w - len([]rune(s)); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

// truncate - Cuts s to w columns, rune-aware, with an ellipsis where one
// fits. Counting runes rather than bytes is what keeps a non-ASCII provider or
// model name from tearing the table's columns apart.
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
