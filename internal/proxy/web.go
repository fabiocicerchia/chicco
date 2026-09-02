package proxy

import (
	_ "embed"
	"net/http"
)

// dashboardHTML is the whole browser dashboard — markup, CSS and script in one
// self-contained page, so /dashboard needs no asset routes and no CDN. It lives
// in its own .html file and is compiled in: as a Go string literal it was 383
// lines no editor could highlight or check, and it is page content, not program
// logic. //go:embed keeps it part of the build, so `go install` still produces
// one binary with nothing to deploy beside it.
//
//go:embed dashboard.html
var dashboardHTML []byte

// handleDashboard - Serves the self-contained HTML web dashboard that mirrors
// the TUI. The page polls /v1/status every second and renders the provider
// table and log pane in the browser without any external dependencies.
func handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(dashboardHTML)
}
