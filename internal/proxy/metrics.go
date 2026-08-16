package proxy

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Prometheus exposition for the counters the dashboard already keeps.
//
// Written by hand rather than pulled from client_golang: the exposition format
// is a dozen lines of text, and chicco's dependency list is deliberately short.
// The tradeoff is that these counters are not the client library's — no
// exemplars, no native histograms — which is the right trade for a proxy whose
// whole job is to be a single small binary.
//
// Every label here is bounded by configuration: provider names and model ids
// come from chicco.yaml, reasons and statuses from a fixed set. Nothing
// request-derived is ever a label — a caller who could put a value in a label
// could grow the series count without limit, and prompts or keys must never
// reach a scrape target at all.

// latencyBuckets are the upper bounds, in seconds, of the upstream latency
// histogram. Wide because the spread is wide: a cached-token completion answers
// in tens of milliseconds, a long generation through a CLI backend can take a
// minute.
var latencyBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

type histogram struct {
	counts []uint64 // one per latencyBuckets entry, cumulative at scrape time
	sum    float64
	total  uint64
}

func (h *histogram) observe(seconds float64) {
	if h.counts == nil {
		h.counts = make([]uint64, len(latencyBuckets))
	}
	h.sum += seconds
	h.total++
	for i, ub := range latencyBuckets {
		if seconds <= ub {
			h.counts[i]++
		}
	}
}

// providerModel keys a counter by the two labels this package exposes.
type providerModel struct{ a, b string }

type metrics struct {
	mu       sync.Mutex
	requests map[providerModel]uint64 // provider, model
	tokens   map[providerModel]uint64 // provider, model
	errors   map[providerModel]uint64 // provider, status ("timeout" for a transport error)
	blocks   map[providerModel]uint64 // provider, reason
	latency  map[string]*histogram    // provider
}

func newMetrics() *metrics {
	return &metrics{
		requests: map[providerModel]uint64{},
		tokens:   map[providerModel]uint64{},
		errors:   map[providerModel]uint64{},
		blocks:   map[providerModel]uint64{},
		latency:  map[string]*histogram{},
	}
}

// observeRequest records one successful upstream call.
func (m *metrics) observeRequest(provider, model string, d time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[providerModel{provider, model}]++
	h, ok := m.latency[provider]
	if !ok {
		h = &histogram{}
		m.latency[provider] = h
	}
	h.observe(d.Seconds())
}

// observeTokens adds to the token counter. Separate from observeRequest because
// the count only exists once the response has been streamed to the caller.
func (m *metrics) observeTokens(provider, model string, tokens int64) {
	if m == nil || tokens <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[providerModel{provider, model}] += uint64(tokens)
}

// observeError records a failed upstream call. status is the HTTP status as a
// string, or "transport" when the request never got one.
func (m *metrics) observeError(provider, status string, d time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errors[providerModel{provider, status}]++
	h, ok := m.latency[provider]
	if !ok {
		h = &histogram{}
		m.latency[provider] = h
	}
	h.observe(d.Seconds())
}

// observeBlock records a provider entering cooldown. reason is chicco's own
// classification: "limit", "auth" or "error".
func (m *metrics) observeBlock(provider, reason string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blocks[providerModel{provider, reason}]++
}

// escape quotes a label value per the exposition format. Provider names and
// model ids come from YAML and are not expected to contain any of this, but a
// scrape target that can be corrupted by a config typo is not worth shipping.
func escape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

// writeCounter emits one counter family, sorted so the output is stable — a
// diffable /metrics is worth the sort on a map this small.
func writeCounter(w io.Writer, name, help, l1, l2 string, vals map[providerModel]uint64) {
	if len(vals) == 0 {
		return
	}
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	keys := make([]providerModel, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].a != keys[j].a {
			return keys[i].a < keys[j].a
		}
		return keys[i].b < keys[j].b
	})
	for _, k := range keys {
		fmt.Fprintf(w, "%s{%s=\"%s\",%s=\"%s\"} %d\n", name, l1, escape(k.a), l2, escape(k.b), vals[k])
	}
}

// write renders the whole exposition. blocked is passed in rather than read
// here: it lives on the Rotator behind its own mutex, and taking both locks in
// one place would invite the lock-order bug this package does not otherwise
// have.
func (m *metrics) write(w io.Writer, blocked int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	writeCounter(w, "chicco_requests_total", "Successful upstream requests, by provider and model.", "provider", "model", m.requests)
	writeCounter(w, "chicco_tokens_total", "Tokens reported by upstream, by provider and model.", "provider", "model", m.tokens)
	writeCounter(w, "chicco_upstream_errors_total", "Failed upstream requests, by provider and HTTP status (\"transport\" when there was none).", "provider", "status", m.errors)
	writeCounter(w, "chicco_provider_blocks_total", "Times a provider entered cooldown, by reason.", "provider", "reason", m.blocks)

	if len(m.latency) > 0 {
		const n = "chicco_upstream_latency_seconds"
		fmt.Fprintf(w, "# HELP %s Upstream call latency, successful or not.\n# TYPE %s histogram\n", n, n)
		names := make([]string, 0, len(m.latency))
		for p := range m.latency {
			names = append(names, p)
		}
		sort.Strings(names)
		for _, p := range names {
			h := m.latency[p]
			for i, ub := range latencyBuckets {
				fmt.Fprintf(w, "%s_bucket{provider=\"%s\",le=\"%s\"} %d\n",
					n, escape(p), strconv.FormatFloat(ub, 'g', -1, 64), h.counts[i])
			}
			fmt.Fprintf(w, "%s_bucket{provider=\"%s\",le=\"+Inf\"} %d\n", n, escape(p), h.total)
			fmt.Fprintf(w, "%s_sum{provider=\"%s\"} %s\n", n, escape(p), strconv.FormatFloat(h.sum, 'g', -1, 64))
			fmt.Fprintf(w, "%s_count{provider=\"%s\"} %d\n", n, escape(p), h.total)
		}
	}

	fmt.Fprint(w, "# HELP chicco_providers_blocked Providers currently in cooldown.\n"+
		"# TYPE chicco_providers_blocked gauge\n")
	fmt.Fprintf(w, "chicco_providers_blocked %d\n", blocked)
}

// blockedCount is the number of keys still in cooldown right now, which is what
// the gauge reports. Expired entries are left in the map by design (block()
// overwrites them), so they are filtered here rather than counted.
func (r *Rotator) blockedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	n := 0
	for _, until := range r.blocked {
		if until.After(now) {
			n++
		}
	}
	return n
}

// MetricsHandler serves the exposition. Deliberately not mounted on the proxy's
// own mux: the proxy port is the one an agent runner points at, and a scrape
// target is a different audience with a different exposure. It carries no auth
// of its own for the same reason the health endpoint does not — bind it
// somewhere only the scraper can reach.
func (r *Rotator) MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.metrics.write(w, r.blockedCount())
	})
}
