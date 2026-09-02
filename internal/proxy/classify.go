package proxy

import (
	"cmp"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// classify.go reads one non-2xx upstream reply — status, Retry-After and error
// text — and says how long to skip the provider and what to call it.

const (
	// defaultCooldown skips a provider after a transient failure (5xx, network,
	// or a 429 with no Retry-After). authCooldown is longer because a rejected key
	// (401/403) won't fix itself in seconds.
	defaultCooldown = time.Minute
	authCooldown    = time.Hour
	// requestErrorCooldown is used for request-shaped 4xx (400/404/413/422): the
	// payload, not the provider, was rejected, so a healthy provider must not be
	// locked out for future (differently-sized) requests. It's just long enough to
	// exclude the provider from the *current* request's failover loop.
	// ponytail: 5s covers a normal fast-failing loop; a provider that takes >5s to
	// 4xx could be re-tried once within the same request — harmless, the loop is
	// bounded by the provider count.
	requestErrorCooldown = 5 * time.Second
)

// isAuth - Reports whether a status means the key was rejected (401/403), as
// opposed to a rate-limit or transient failure.
func isAuth(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// blockReason - Maps an upstream status to a cooldown reason for the dashboard.
func blockReason(status int) string {
	switch {
	case isAuth(status):
		return "auth"
	case status == http.StatusTooManyRequests:
		return "limit"
	default:
		return "error"
	}
}

// isRequestError - Reports whether a status means the request itself was
// rejected (bad body, unknown model, payload too large) rather than the
// provider being unhealthy or rate-limited. Waiting won't help these, and they
// say nothing about whether the next request would succeed — so they get only a
// brief cooldown.
func isRequestError(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound,
		http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// cooldown - Picks how long to skip a provider after a failure: a rejected key
// for an hour, a brief skip for a request-shaped 4xx (the payload was at fault,
// not the provider), an explicit Retry-After when given, otherwise a short
// default.
func cooldown(status int, retryAfter time.Duration) time.Duration {
	if isAuth(status) {
		return authCooldown
	}
	if isRequestError(status) {
		return requestErrorCooldown
	}
	if retryAfter > 0 {
		return retryAfter
	}
	return defaultCooldown
}

// quotaBodyRe / modelBodyRe read an upstream's error TEXT, because the status
// alone is ambiguous: 403 is used for a rejected key, for "free quota
// exhausted" (Alibaba) and for "model is not available on this plan"
// (Cloudflare). Treating all three as an auth failure cooled the whole provider
// for an hour and greyed it as "invalid key", which took every OTHER model on
// that provider down with it — one wrong model id in the config was enough to
// lose five working ones.
var (
	quotaBodyRe = regexp.MustCompile(`(?i)(quota|credit|billing|insufficient|exhaust|payment|spend|top ?up|upgrade)`)
	// `.` not `[^.]`: model ids contain dots (@cf/moonshotai/kimi-k2.7-code), which
	// is exactly the id that slipped through and cost the provider an hour.
	modelBodyRe = regexp.MustCompile(`(?i)model.{0,100}?(not available|not found|does not exist|unsupported|invalid|no access|not supported)` +
		`|(?i)(invalid|unknown|unsupported).{0,20}model`)
)

// upstreamClass is how one non-2xx upstream reply should be handled: how long to
// skip, what to call it on the dashboard, and whether the skip applies to the
// whole provider or only the model that was asked for.
type upstreamClass struct {
	reason      string // "auth" | "limit" | "error"
	cooldown    time.Duration
	modelScoped bool // block "provider/model", leaving sibling models routable
}

// classifyUpstream - Decides that, from the status, the Retry-After header and
// the error body. Body text is only ever used to make a verdict LESS severe
// than the status alone would imply, so a provider that says nothing useful
// behaves exactly as before.
func classifyUpstream(status int, body string, retryAfter time.Duration) upstreamClass {
	// Quota wording is checked FIRST and stays provider-wide: an exhausted account
	// affects every model on it, and such messages often mention "model" too
	// ("...to keep using the model, upgrade"), which must not scope it to one model.
	switch {
	case status == http.StatusPaymentRequired: // 402 — out of credits
		return upstreamClass{"limit", cmp.Or(retryAfter, authCooldown), false}
	case status == http.StatusForbidden && quotaBodyRe.MatchString(body):
		// "free quota has been exhausted" — a real limit, not a bad key. Labelled
		// honestly so the dashboard stops claiming the API key is invalid.
		return upstreamClass{"limit", cmp.Or(retryAfter, authCooldown), false}
	}
	// A complaint about the named model is about the request, not the provider:
	// skip just this model, briefly, and leave its siblings routable.
	if modelBodyRe.MatchString(body) && (isAuth(status) || isRequestError(status)) {
		return upstreamClass{"error", requestErrorCooldown, true}
	}
	return upstreamClass{blockReason(status), cooldown(status, retryAfter), false}
}

// parseRetryAfter - Reads a Retry-After header (delta-seconds form) into a
// duration; 0 when absent or not a plain number.
func parseRetryAfter(h string) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}
