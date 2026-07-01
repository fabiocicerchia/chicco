package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// CLI-backed providers (kind: cli).
//
// Instead of an HTTP call, a CLI provider runs a local tool (claude, codex, kiro,
// a qwen CLI, …) as a one-shot subprocess: chicco builds the argv from a template,
// feeds it the prompt, buffers the whole completion, then synthesizes the
// OpenAI-compatible SSE the caller expects. The Rotator's
// cooldown/health/usage machinery is reused unchanged — a non-zero exit looks like
// an HTTP 5xx and fails over to the next provider, which is clean because nothing
// has been written to the client yet.

// cliDefaultTimeout bounds a CLI run when the provider sets no timeout_seconds.
const cliDefaultTimeout = 120 * time.Second

// ansiRe strips terminal colour/escape codes from CLI output (e.g. kiro).
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]")

// authFailureRe matches the messages CLIs print when they aren't logged in, so a
// logged-out tool greys in the dashboard (HealthAuth) rather than just cooling
// down. It only runs on an already-failed call, so a false positive merely picks
// the longer cooldown.
var authFailureRe = regexp.MustCompile(`(?i)(not logged in|/login|log ?in|sign ?in|unauthenticated|unauthorized|expired|invalid (api )?key|no credentials|forbidden|\b40[13]\b)`)

// rateLimitRe matches the messages CLIs print when a usage window is exhausted, so
// the provider is cooled down until the window reopens (parseResetDuration) rather
// than retried in a minute.
var rateLimitRe = regexp.MustCompile(`(?i)(rate.?limit|usage limit|limit reached|reached your|too many requests|quota|credits?\s*(exhausted|used up|remaining: ?0)|out of (credits|messages)|try again|resets?\b)`)

// rateLimitCooldown is the fallback cooldown when a CLI says it's limited but gives
// no parseable reset time.
const rateLimitCooldown = time.Hour

// cliFailure wraps a failed CLI run as a non-2xx upstream so handleChat cools the
// provider down and fails over: 401 for an auth problem (greys the provider, long
// cooldown); 429 with the parsed reset time for a usage-limit hit (so the dashboard
// shows when the next window opens); otherwise a transient 502.
func cliFailure(msg string) *upstream {
	switch {
	case authFailureRe.MatchString(msg):
		return &upstream{status: http.StatusUnauthorized, body: io.NopCloser(strings.NewReader(msg))}
	case rateLimitRe.MatchString(msg):
		d := parseResetDuration(msg)
		if d <= 0 {
			d = rateLimitCooldown
		}
		return &upstream{status: http.StatusTooManyRequests, retryAfter: d, body: io.NopCloser(strings.NewReader(msg))}
	default:
		return &upstream{status: http.StatusBadGateway, body: io.NopCloser(strings.NewReader(msg))}
	}
}

var (
	resetUnitRe  = regexp.MustCompile(`(\d+)\s*(hours?|hrs?|minutes?|mins?|seconds?|secs?|[hms])\b`)
	resetClockRe = regexp.MustCompile(`(\d{1,2})(?::(\d{2}))?\s*([ap]m)?`)
)

// parseResetDuration best-effort extracts how long until a CLI's usage window
// reopens, from phrasing like "resets in 2h 30m", "try again in 45 minutes", or
// "resets at 3pm". It anchors on the reset/again clause so a window *length* (e.g.
// "5-hour limit") isn't mistaken for the reset time. Returns 0 when nothing parses.
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

// clockReset returns the duration until the next occurrence of a clock time like
// "3pm" or "15:00" (local time). 0 when it can't parse.
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

// runCLI executes a CLI provider for one request and returns its reply as an
// upstream. A failed run (non-zero exit, timeout, missing binary) is reported as a
// 502 so the caller cools the provider down and rotates, rather than as a hard
// error — the behaviour matches a flaky HTTP upstream.
func runCLI(ctx context.Context, p Provider, model string, payload map[string]any) (*upstream, error) {
	system, user := splitMessages(payload)
	prompt := user
	if system != "" {
		prompt = system + "\n\n" + user
	}

	// Some CLIs (codex) write their answer to a file rather than stdout.
	var outFile string
	if p.OutputFile {
		f, err := os.CreateTemp("", "chicco-cli-*.out")
		if err != nil {
			return nil, fmt.Errorf("temp file: %w", err)
		}
		outFile = f.Name()
		f.Close()
		defer os.Remove(outFile)
	}

	repl := strings.NewReplacer(
		"{{model}}", model,
		"{{system}}", system,
		"{{user}}", user,
		"{{prompt}}", prompt,
		"{{output_file}}", outFile,
	)
	args := make([]string, len(p.Args))
	for i, a := range p.Args {
		args[i] = repl.Replace(a)
	}

	timeout := cliDefaultTimeout
	if p.TimeoutSecs > 0 {
		timeout = time.Duration(p.TimeoutSecs) * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, p.Command, args...)
	if p.PromptStdin {
		cmd.Stdin = strings.NewReader(prompt)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return cliFailure(msg), nil
	}

	raw := stdout.Bytes()
	if p.OutputFile {
		raw, _ = os.ReadFile(outFile)
	}
	text, tokens, failed := extractCompletion(p, raw)
	if failed {
		// The tool ran but reported an error in its output (e.g. claude's
		// "is_error": true on a logged-out / rate-limited call). Treat it as a
		// failed upstream so the request cools the provider down and fails over,
		// instead of serving the error string as the completion.
		return cliFailure("CLI reported error: " + text), nil
	}
	if p.StripANSI {
		text = ansiRe.ReplaceAllString(text, "")
	}
	text = strings.TrimSpace(text)
	if tokens == 0 {
		tokens = int64(len(text) / 4) // ponytail: rough estimate when the CLI reports none
	}

	return &upstream{
		status:      http.StatusOK,
		contentType: "text/event-stream",
		body:        io.NopCloser(bytes.NewReader(synthSSE(text, tokens))),
	}, nil
}

// splitMessages pulls the system prompt and the joined user/assistant turns out of
// an OpenAI messages array (decoded as a map).
func splitMessages(payload map[string]any) (system, user string) {
	msgs, _ := payload["messages"].([]any)
	var turns []string
	for _, mi := range msgs {
		m, _ := mi.(map[string]any)
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		if role == "system" {
			if system != "" {
				system += "\n\n"
			}
			system += content
		} else {
			turns = append(turns, content)
		}
	}
	return system, strings.Join(turns, "\n\n")
}

// extractCompletion pulls the reply text (and any reported token count) out of a
// CLI's raw output: a dotted path into a JSON object for Output=="json", else the
// raw stdout verbatim. failed is true when ErrorPath is set and truthy in the JSON.
func extractCompletion(p Provider, raw []byte) (text string, tokens int64, failed bool) {
	if p.Output != "json" {
		return string(raw), 0, false
	}
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(raw), &m); err != nil {
		return string(raw), 0, false // not JSON after all — fall back to raw
	}
	text = asString(dotGet(m, p.ResultPath))
	if p.ErrorPath != "" && truthy(dotGet(m, p.ErrorPath)) {
		return text, 0, true
	}
	if p.TokensPath != "" {
		tokens = asInt64(dotGet(m, p.TokensPath))
	}
	return text, tokens, false
}

// truthy reports whether a decoded JSON value signals "yes/error": a true bool, a
// non-zero number, or a non-empty/"false" string.
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != "" && t != "false"
	default:
		return false
	}
}

// dotGet walks a dotted path (e.g. "usage.output_tokens") into a decoded JSON map.
func dotGet(m map[string]any, path string) any {
	if path == "" {
		return nil
	}
	var cur any = m
	for _, part := range strings.Split(path, ".") {
		mp, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mp[part]
	}
	return cur
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

func asInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	default:
		return 0
	}
}

// synthSSE renders a completion as the minimal OpenAI SSE stream a client accepts:
// one content delta, an optional usage chunk (for the dashboard bar), and [DONE].
func synthSSE(text string, tokens int64) []byte {
	var b bytes.Buffer
	chunk, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": text}}},
	})
	b.WriteString("data: ")
	b.Write(chunk)
	b.WriteString("\n\n")
	if tokens > 0 {
		usage, _ := json.Marshal(map[string]any{
			"choices": []any{},
			"usage":   map[string]any{"total_tokens": tokens},
		})
		b.WriteString("data: ")
		b.Write(usage)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.Bytes()
}
