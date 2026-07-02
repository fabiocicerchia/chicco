package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Manual "test all models" probe (the `t` key in the dashboard).
//
// runTest sends a "hello world" prompt to every configured model and logs which
// answered and each provider's window/limit state into the log pane — a way to
// refresh the stats on demand without leaving chicco. It probes each (provider,
// model) directly, not through the rotation, so a model in cooldown is still tried
// (that's how you see its real state). Results fold back into the rotator
// (cooldown/health/usage) so the table and state file agree afterwards. It runs in
// the background (slow: one real call per model, CLIs spawn a subprocess each).

// testRunning guards against overlapping runs (a double key-press).
var testRunning atomic.Bool

type testResult struct {
	ok         bool
	text       string
	tokens     int64
	status     int
	retryAfter time.Duration
	errMsg     string
}

func runTest(rot *Rotator) {
	if !testRunning.CompareAndSwap(false, true) {
		log.Printf("chicco test: a test run is already in progress")
		return
	}
	defer testRunning.Store(false)

	rot.CheckHealth(context.Background())
	providers := rot.Active()
	if len(providers) == 0 {
		log.Printf("chicco test: no providers configured")
		return
	}
	total := 0
	for _, p := range providers {
		total += len(p.Models)
	}
	log.Printf("chicco test: probing %d model(s) with a hello-world prompt…", total)

	ok := 0
	for _, p := range providers {
		for _, model := range p.Models {
			res := testOne(context.Background(), p, model)
			if res.ok {
				ok++
				rot.recordUsage(p.Name, model, res.tokens)
				rot.setHealth(p.Name, HealthOK)
				log.Printf("chicco test: ✓ %s/%s — %d tok · %s", p.Name, model, res.tokens, windowDesc(p, rot))
			} else {
				rot.block(p.Name, cooldown(res.status, res.retryAfter), blockReason(res.status))
				if isAuth(res.status) {
					rot.setHealth(p.Name, HealthAuth)
				}
				log.Printf("chicco test: ✗ %s/%s — %s · %s", p.Name, model, failDetail(res), windowDesc(p, rot))
			}
		}
	}
	log.Printf("chicco test: done — %d/%d model(s) responded", ok, total)
}

// testOne sends the hello-world prompt to one provider/model and reports the outcome.
func testOne(ctx context.Context, p Provider, model string) testResult {
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	payload := map[string]any{
		"model":    model,
		"stream":   false,
		"messages": []any{map[string]any{"role": "user", "content": "Reply with exactly: hello world"}},
	}
	var up *upstream
	var err error
	if p.Kind == "cli" {
		up, err = runCLI(cctx, p, model, payload)
	} else {
		body, _ := json.Marshal(payload)
		up, err = forward(cctx, p, body, "/chat/completions")
	}
	if err != nil {
		return testResult{status: http.StatusBadGateway, errMsg: err.Error()}
	}
	if up.status < 200 || up.status >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(up.body, 300))
		up.body.Close()
		return testResult{status: up.status, retryAfter: up.retryAfter, errMsg: strings.TrimSpace(string(snippet))}
	}
	text, tokens := drainCompletion(up)
	return testResult{ok: true, text: text, tokens: tokens}
}

// drainCompletion reads a non-streamed JSON body or synthesized SSE and returns the
// reply text and any reported token count.
func drainCompletion(up *upstream) (string, int64) {
	defer up.body.Close()
	var content strings.Builder
	var tokens int64
	br := bufio.NewReaderSize(up.body, 1024*1024)
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 {
			content.WriteString(contentDelta(line))
			if t := usageTokens(line); t > 0 {
				tokens = t
			}
		}
		if rerr != nil {
			break
		}
	}
	return strings.TrimSpace(content.String()), tokens
}

// contentDelta extracts assistant text from one response line — a streamed
// delta.content or a non-streamed message.content.
func contentDelta(line []byte) string {
	data := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
	if len(data) == 0 || data[0] != '{' {
		return ""
	}
	var c struct {
		Choices []struct {
			Delta   struct{ Content string } `json:"delta"`
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &c) != nil || len(c.Choices) == 0 {
		return ""
	}
	if c.Choices[0].Delta.Content != "" {
		return c.Choices[0].Delta.Content
	}
	return c.Choices[0].Message.Content
}

// failDetail summarises why a model didn't answer (single line).
func failDetail(res testResult) string {
	switch blockReason(res.status) {
	case "auth":
		return "auth failed — login required"
	case "limit":
		if res.retryAfter > 0 {
			return "limit — resets in " + res.retryAfter.Round(time.Minute).String()
		}
		return "limit reached"
	default:
		return "error: " + truncate(strings.Join(strings.Fields(res.errMsg), " "), 60)
	}
}

// windowDesc describes a provider's quota window after the test: current usage vs
// quota, the limit reset, or "no quota" for subscription tools.
func windowDesc(p Provider, rot *Rotator) string {
	var stat ProviderStat
	for _, s := range rot.Snapshot() {
		if s.Name == p.Name {
			stat = s
			break
		}
	}
	quota, quotaIsTokens, quotaWindow := p.effectiveQuota()
	switch {
	case stat.CooldownLeft > 0 && stat.CooldownKind == "limit":
		return "LIMIT — resets in " + stat.CooldownLeft.Round(time.Minute).String()
	case quota > 0 && quotaIsTokens:
		return fmt.Sprintf("%s/%s tok · %s", fmtTok(stat.UsedTokens), fmtTok(quota), quotaWindow)
	case quota > 0:
		return fmt.Sprintf("%d/%d req · %s", stat.Requests, quota, quotaWindow)
	default:
		return "no tracked quota (subscription / per-token)"
	}
}
