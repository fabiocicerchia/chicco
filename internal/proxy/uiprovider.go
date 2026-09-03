package proxy

import (
	"fmt"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// uiprovider.go turns one provider's snapshot into the table rows uirender.go
// lays out: the status dot and bar on the provider's own row, then one row per
// model underneath it.

// providerRows - Formats a provider's stat as one row per model. The first row
// carries the provider name, status dot, provider-level usage figures and bar;
// each subsequent model row shows the model name and its own token/request
// counts and bar (scaled against the same provider quota so you can compare).
func providerRows(s ProviderStat, width int) []string {
	q := newUsageBar(s, width)
	dot, tail := providerStatus(s, q)

	models := s.Models
	if len(models) == 0 {
		models = []ModelStat{{Name: "—"}}
	}

	rows := make([]string, 0, len(models))
	for i, ms := range models {
		if i == 0 {
			// First row: provider name + dot + kind + provider-level usage.
			rows = append(rows, modelRow(dot, s.Name, s.Kind, ms.Name,
				q.usage(s.UsedTokens, s.Requests),
				fmt.Sprintf("req %d", s.Requests),
				tail, width, false))
			continue
		}
		// Continuation rows: blank name/kind, model name in the MODEL column —
		// same column as the header and the first row, so it lines up with every
		// other model name in the table.
		usage, mtail := modelCells(ms, q)
		rows = append(rows, modelRow(" ", "", "", ms.Name,
			usage,
			fmt.Sprintf("req %d", ms.Requests),
			mtail, width, false))
	}
	return rows
}

// usageBar renders usage figures and bars against the quota governing one
// provider: tokens take priority over request count, and a provider with
// neither gets "(no quota)" where its bar would be. width is fixed at
// construction so every row of the same provider draws the same-length bar.
type usageBar struct {
	quota     int64
	byTokens  bool
	byRequest bool
	width     int
}

// newUsageBar - Resolves a provider's governing quota and the width its bars
// get, once, for all of that provider's rows.
func newUsageBar(s ProviderStat, width int) usageBar {
	// Reserve room for a "  cd 47s" suffix when in cooldown so the bar still fits.
	reserve := 0
	if s.CooldownLeft > 0 {
		reserve = 12
	}
	barW := width - 20 - 5 - 24 - 18 - 9 - 8 - reserve
	if barW < 6 {
		barW = 6
	}
	if barW > 64 {
		barW = 64
	}
	return usageBar{
		quota:     s.Quota,
		byTokens:  s.QuotaIsTokens && s.Quota > 0,
		byRequest: !s.QuotaIsTokens && s.Quota > 0,
		width:     barW,
	}
}

// usage - Renders the "USED / QUOTA" cell for a row.
func (q usageBar) usage(tokens int64, reqs int) string {
	switch {
	case q.byTokens:
		return fmt.Sprintf("%s / %s", fmtTok(tokens), fmtTok(q.quota))
	case q.byRequest:
		return fmt.Sprintf("%d / %d req", reqs, int(q.quota))
	default:
		return fmt.Sprintf("%s / —", fmtTok(tokens))
	}
}

// barAt - Renders the bar and its percentage for an already-computed fraction.
func (q usageBar) barAt(pct float64) string {
	return renderBar(pct, q.width) + fmt.Sprintf(" %3.0f%%", pct*100)
}

// barFor - Renders the bar for a row's usage against the governing quota, or the
// "(no quota)" placeholder when there is none to measure against.
func (q usageBar) barFor(tokens int64, reqs int) string {
	switch {
	case q.byTokens:
		return q.barAt(float64(tokens) / float64(q.quota))
	case q.byRequest:
		return q.barAt(float64(reqs) / float64(q.quota))
	default:
		return dimStyle.Render("(no quota)")
	}
}

// providerStatus - Picks the status dot and the trailing cell for a provider's
// own row: a usage bar when it is servable, otherwise the reason it isn't.
func providerStatus(s ProviderStat, q usageBar) (dot, tail string) {
	grey := lipgloss.NewStyle().Foreground(colGrey)
	green := lipgloss.NewStyle().Foreground(colGreen)
	amber := lipgloss.NewStyle().Foreground(colAmber)

	switch {
	case s.Inactive:
		return grey.Render("●"), grey.Render("not configured — check api_key/models")
	case s.Health == HealthAuth:
		return grey.Render("●"), grey.Render("auth failed — check API key")
	case s.Health == HealthDown:
		return grey.Render("●"), grey.Render("unreachable")
	case s.Health == HealthUnknown:
		return grey.Render("○"), grey.Render("checking…")
	case s.CooldownLeft > 0 && s.CooldownKind == "limit":
		return amber.Render("◐"), amber.Render("limit · resets " + fmtReset(s.CooldownLeft))
	case s.CooldownLeft > 0:
		return amber.Render("◐"), q.barFor(s.UsedTokens, s.Requests) +
			amber.Render("  cd "+s.CooldownLeft.Round(time.Second).String())
	default:
		return green.Render("●"), q.barFor(s.UsedTokens, s.Requests)
	}
}

// modelCells - Renders the usage cell and bar for one model's continuation row.
// A model with its own quota is measured against that; otherwise it falls back
// to the provider-level quota so the bar stays comparable down the column.
func modelCells(ms ModelStat, q usageBar) (usage, tail string) {
	if ms.Quota <= 0 {
		return q.usage(ms.Tokens, ms.Requests), q.barFor(ms.Tokens, ms.Requests)
	}
	if ms.QuotaIsTokens {
		return fmt.Sprintf("%s / %s", fmtTok(ms.UsedTokens), fmtTok(ms.Quota)),
			q.barAt(float64(ms.UsedTokens) / float64(ms.Quota))
	}
	return fmt.Sprintf("%d / %d req", ms.Requests, int(ms.Quota)),
		q.barAt(float64(ms.Requests) / float64(ms.Quota))
}
