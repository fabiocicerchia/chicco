package proxy

import (
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// clierrors.go reads a CLI provider's failure output: whether it is a logout or
// a rate limit, and how long the tool says to wait.

// ansiRe strips terminal colour/escape codes from CLI output (e.g. kiro).
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]")

// authFailureRe matches the messages CLIs print when they aren't logged in, so a
// logged-out tool greys in the dashboard (HealthAuth) rather than just cooling
// down. It only runs on an already-failed call, so a false positive merely picks
// the longer cooldown.
// "error authenticating" / "no longer supported" / "ineligible" are in there for
// the shape gemini-cli fails with when Google drops a client or tier: an auth
// error that never says "login", and so used to read as a transient 502 and be
// retried every minute forever instead of greying out.
var authFailureRe = regexp.MustCompile(`(?i)(not logged in|/login|log ?in|sign ?in|unauthenticat|authenticating|authentication (failed|error)|ineligible|no longer supported|unauthorized|expired|invalid (api )?key|no credentials|forbidden|\b40[13]\b)`)

// rateLimitRe matches the messages CLIs print when a usage window is exhausted, so
// the provider is cooled down until the window reopens (parseResetDuration) rather
// than retried in a minute.
var rateLimitRe = regexp.MustCompile(`(?i)(rate.?limit|usage limit|limit reached|reached your|too many requests|quota|credits?\s*(exhausted|used up|remaining: ?0)|out of (credits|messages)|try again|resets?\b)`)

// rateLimitCooldown is the fallback cooldown when a CLI says it's limited but gives
// no parseable reset time.
const rateLimitCooldown = time.Hour

// stackFrameRe matches a JS/Python-style stack frame line, which is what fills a
// Node CLI's stderr and pushes the actual error out of cliErrSnippet.
var stackFrameRe = regexp.MustCompile(`(?m)^\s+at .*\n?`)

// cliFailure - Wraps a failed CLI run as a non-2xx upstream so handleChat cools
// the provider down and fails over: 401 for an auth problem (greys the
// provider, long cooldown); 429 with the parsed reset time for a usage-limit
// hit (so the dashboard shows when the next window opens); otherwise a
// transient 502.
func cliFailure(msg string) *upstream {
	// Classify on the WHOLE message, report the useful part of it. The caller
	// only ever reads the first 512 bytes of an error body (see dispatch), and a
	// Node CLI spends most of those on a stack trace: gemini-cli leads with an
	// "Approval mode overridden" notice, then a dozen `at …` frames, and only
	// then says it failed to authenticate. Head-first, that snippet showed the
	// warning and nothing else. Drop the frames and keep the tail.
	body := strings.TrimSpace(stackFrameRe.ReplaceAllString(msg, ""))
	if len(body) > cliErrSnippet {
		body = "…" + body[len(body)-cliErrSnippet:]
	}
	switch {
	case authFailureRe.MatchString(msg):
		return &upstream{status: http.StatusUnauthorized, body: io.NopCloser(strings.NewReader(body))}
	case rateLimitRe.MatchString(msg):
		d := parseResetDuration(msg)
		if d <= 0 {
			d = rateLimitCooldown
		}
		return &upstream{status: http.StatusTooManyRequests, retryAfter: d, body: io.NopCloser(strings.NewReader(body))}
	default:
		return &upstream{status: http.StatusBadGateway, body: io.NopCloser(strings.NewReader(body))}
	}
}

var (
	resetUnitRe  = regexp.MustCompile(`(\d+)\s*(hours?|hrs?|minutes?|mins?|seconds?|secs?|[hms])\b`)
	resetClockRe = regexp.MustCompile(`(\d{1,2})(?::(\d{2}))?\s*([ap]m)?`)
)

// parseResetDuration - Best-effort extracts how long until a CLI's usage window
// reopens, from phrasing like "resets in 2h 30m", "try again in 45 minutes", or
// "resets at 3pm". It anchors on the reset/again clause so a window *length*
// (e.g. "5-hour limit") isn't mistaken for the reset time. Returns 0 when
// nothing parses.
func parseResetDuration(msg string) time.Duration {
	m := strings.ToLower(msg)
	clause := ""
	for _, kw := range []string{"reset", "try again", "again", "available"} {
		if i := strings.Index(m, kw); i >= 0 {
			clause = m[i:]
			break
		}
	}
	if clause == "" {
		return 0
	}
	// Relative: "... in 2h 30m" / "in 45 minutes".
	if strings.Contains(clause, " in ") || strings.HasPrefix(clause, "in ") {
		var total time.Duration
		for _, u := range resetUnitRe.FindAllStringSubmatch(clause, -1) {
			n, _ := strconv.Atoi(u[1])
			switch u[2][0] {
			case 'h':
				total += time.Duration(n) * time.Hour
			case 'm':
				total += time.Duration(n) * time.Minute
			case 's':
				total += time.Duration(n) * time.Second
			}
		}
		if total > 0 {
			return total
		}
	}
	// Absolute clock: "... at 3pm" / "at 15:00".
	if i := strings.Index(clause, "at "); i >= 0 {
		return clockReset(clause[i+3:])
	}
	return 0
}

// clockReset - Returns the duration until the next occurrence of a clock time
// like "3pm" or "15:00" (local time). 0 when it can't parse.
func clockReset(s string) time.Duration {
	m := resetClockRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil || m[1] == "" {
		return 0
	}
	hour, _ := strconv.Atoi(m[1])
	min := 0
	if m[2] != "" {
		min, _ = strconv.Atoi(m[2])
	}
	switch m[3] {
	case "pm":
		if hour < 12 {
			hour += 12
		}
	case "am":
		if hour == 12 {
			hour = 0
		}
	}
	if hour > 23 || min > 59 {
		return 0
	}
	now := time.Now()
	target := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location())
	if !target.After(now) {
		target = target.Add(24 * time.Hour) // already passed today → tomorrow
	}
	return time.Until(target)
}
