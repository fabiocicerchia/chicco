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

// cliErrSnippet is how much of a failed upstream's body is kept for the log line
// and the 503 shown to the caller (see dispatch, which reads exactly this much).
const cliErrSnippet = 512

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

// runCLI - Executes a CLI provider for one request and returns its reply as an
// upstream. A failed run (non-zero exit, timeout, missing binary) is reported
// as a 502 so the caller cools the provider down and rotates, rather than as a
// hard error — the behaviour matches a flaky HTTP upstream.
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
		// Only the name is wanted; the CLI reopens and writes it itself.
		_ = f.Close()
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
	text, tokens, promptTokens, failed := extractCompletion(p, raw)
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
	// Prompt tokens: exact when the tool reports them (input_tokens_path — claude
	// and codex both print usage in --output-format json), else estimated.
	//
	// ponytail: 4 chars/token is the fallback, not the plan. Reporting 0
	// input_tokens made every CLI-served reply look free to a caller doing cost
	// accounting; an estimate is wrong by a few percent, 0 is wrong by the whole
	// prompt. Exactness needs the model's own BPE vocab, which differs per model
	// and isn't published for every one of them — configure the path instead.
	if promptTokens == 0 {
		promptTokens = int64(len(prompt) / 4)
	}

	// Answer in the shape the caller asked for. Synthesizing SSE unconditionally
	// handed a `"stream": false` client `Content-Type: text/event-stream` and
	// `data:` frames, which no OpenAI client parses. handleMessages always sets
	// stream=true before dispatch, so the Anthropic path is unaffected.
	if s, _ := payload["stream"].(bool); !s {
		return &upstream{
			status:      http.StatusOK,
			contentType: "application/json",
			body:        io.NopCloser(bytes.NewReader(synthJSON(model, text, promptTokens, tokens))),
		}, nil
	}

	return &upstream{
		status:      http.StatusOK,
		contentType: "text/event-stream",
		body:        io.NopCloser(bytes.NewReader(synthSSE(model, text, promptTokens, tokens))),
	}, nil
}

// splitMessages - Pulls the system prompt and the joined user/assistant turns
// out of an OpenAI messages array (decoded as a map).
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

// extractCompletion - Pulls the reply text (and any reported token counts) out
// of a CLI's raw output: a dotted path into a JSON object for Output=="json",
// else the raw stdout verbatim. failed is true when ErrorPath is set and truthy
// in the JSON. inTokens is 0 unless the tool reports prompt usage at
// InTokensPath, which the caller then prefers over its own estimate.
func extractCompletion(p Provider, raw []byte) (text string, tokens, inTokens int64, failed bool) {
	if p.Output != "json" {
		return string(raw), 0, 0, false
	}
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(raw), &m); err != nil {
		return string(raw), 0, 0, false // not JSON after all — fall back to raw
	}
	text = asString(dotGet(m, p.ResultPath))
	if p.ErrorPath != "" && truthy(dotGet(m, p.ErrorPath)) {
		return text, 0, 0, true
	}
	if p.TokensPath != "" {
		tokens = asInt64(dotGet(m, p.TokensPath))
	}
	if p.InTokensPath != "" {
		inTokens = asInt64(dotGet(m, p.InTokensPath))
	}
	return text, tokens, inTokens, false
}

// truthy - Reports whether a decoded JSON value signals "yes/error": a true
// bool, a non-zero number, or a non-empty/"false" string.
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

// dotGet - Walks a dotted path (e.g. "usage.output_tokens") into a decoded JSON
// map.
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

// asString - Coerces a decoded JSON value to a string. CLI providers emit
// hand-rolled JSON, so a field that should be a string arrives as a number or
// is missing entirely often enough to be worth absorbing here rather than at
// every call site.
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

// asInt64 - Coerces a decoded JSON number to an int64. encoding/json decodes
// every number into a float64, so the int and int64 cases only matter for
// values this package built itself.
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

// synthJSON - Renders a completion as a non-streamed OpenAI chat.completion
// object, for callers that sent "stream": false. Carries the fields strict
// clients require (id/object/created/model/finish_reason) rather than the bare
// choices array the SSE path gets away with.
func synthJSON(model, text string, promptTokens, tokens int64) []byte {
	out, _ := json.Marshal(map[string]any{
		"id":      synthID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": synthUsage(promptTokens, tokens),
	})
	return out
}

// synthSSE - Renders a completion as the minimal OpenAI SSE stream a client
// accepts: one content delta, an optional usage chunk (for the dashboard bar),
// and [DONE]. Every chunk carries id/model because they are what a caller reads
// back to tell WHICH provider served it — /v1/messages relays them as the
// Anthropic response's id and model, which were empty for CLI-served replies
// while these were omitted.
func synthSSE(model, text string, promptTokens, tokens int64) []byte {
	var b bytes.Buffer
	id := synthID()
	created := time.Now().Unix()
	chunk, _ := json.Marshal(map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
	})
	b.WriteString("data: ")
	b.Write(chunk)
	b.WriteString("\n\n")
	if tokens > 0 || promptTokens > 0 {
		usage, _ := json.Marshal(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{},
			"usage":   synthUsage(promptTokens, tokens),
		})
		b.WriteString("data: ")
		b.Write(usage)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.Bytes()
}

// synthID - Mints a response id for a CLI provider, which has none of its own.
// Prefixed so a synthesized id is recognisable as chicco's in a log, and
// nanosecond-based so two replies in the same second do not collide.
func synthID() string {
	return "chatcmpl-chicco-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// synthUsage - Builds the usage block for a CLI provider, which reports no
// token counts. The shape is OpenAI's because everything downstream reads it
// that way; the numbers are estimates, and the rotator treats them as such.
func synthUsage(promptTokens, tokens int64) map[string]any {
	return map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": tokens,
		"total_tokens":      promptTokens + tokens,
	}
}
