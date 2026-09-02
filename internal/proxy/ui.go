package proxy

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ui.go is the Bubble Tea dashboard's model: its state, its key handling and
// the frame it assembles from uirender.go's pieces.

var (
	colGreen = lipgloss.Color("#2ecc71")
	colAmber = lipgloss.Color("#f1c40f")
	colRed   = lipgloss.Color("#e74c3c")
	colGrey  = lipgloss.Color("#7f8c8d")
	colDim   = lipgloss.Color("#5c6370")
	colTitle = lipgloss.Color("#61afef")

	titleStyle  = lipgloss.NewStyle().Foreground(colTitle).Bold(true)
	dimStyle    = lipgloss.NewStyle().Foreground(colDim)
	errLogStyle = lipgloss.NewStyle().Foreground(colRed)
	boxStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colDim).Padding(0, 1)
	headerStyle = lipgloss.NewStyle().Foreground(colDim).Bold(true)
	// scrollThumbStyle is deliberately brighter than the border/dimStyle grey
	// (colDim) so the thumb doesn't blend into the box's own border.
	scrollThumbStyle = lipgloss.NewStyle().Foreground(colGrey)
)

// renderTrack - Styles one scrollbarColumn glyph: the thumb ("█") stands out in
// scrollThumbStyle, the track ("│") and blank column stay dim like the border.
func renderTrack(glyph string) string {
	if glyph == "█" {
		return scrollThumbStyle.Render(glyph)
	}
	return dimStyle.Render(glyph)
}

// ── model ───────────────────────────────────────────────────────────────────

type tickMsg time.Time

// tick - Schedules the next one-second refresh. The dashboard polls rather
// than being pushed to, so a busy proxy cannot flood the UI with redraws.
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

// newUIModel - Builds the dashboard model, focused on the provider table.
func newUIModel(rot *Rotator, logs *logBuffer, addr string) uiModel {
	return uiModel{rot: rot, logs: logs, addr: addr, focus: focusProviders}
}

// Init - Starts the refresh loop. Part of the tea.Model contract.
func (m uiModel) Init() tea.Cmd { return tick() }

// Update - Handles one event — a resize, a key, or the one-second tick — and
// returns the next model. Part of the tea.Model contract.
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

// pageSize - Returns how many lines to jump on pgup/pgdown — half the log pane
// height.
func (m uiModel) pageSize() int {
	logH := m.height * 2 / 5
	p := (logH - 2) / 2
	if p < 1 {
		p = 1
	}
	return p
}

// View - Renders the whole dashboard: the provider table over the log pane,
// the log pane taking two fifths of the height. Part of the tea.Model
// contract.
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
