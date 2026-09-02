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
	"strings"
	"time"
)

// CLI-backed providers (kind: cli).
//
// Instead of an HTTP call, a CLI provider runs a local tool (claude, codex, kiro,
// a qwen CLI, …) as a one-shot subprocess: chicco builds the argv from a template,
// feeds it the prompt, buffers the whole completion, then synthesizes the
// OpenAI-compatible SSE the caller expects (clisynth.go). The Rotator's
// cooldown/health/usage machinery is reused unchanged — a non-zero exit looks like
// an HTTP 5xx and fails over to the next provider, which is clean because nothing
// has been written to the client yet. clierrors.go reads what the tool printed to
// tell a logout from a rate limit.

// cliDefaultTimeout bounds a CLI run when the provider sets no timeout_seconds.
const cliDefaultTimeout = 120 * time.Second

// cliErrSnippet is how much of a failed upstream's body is kept for the log line
// and the 503 shown to the caller (see dispatch, which reads exactly this much).
const cliErrSnippet = 512

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
