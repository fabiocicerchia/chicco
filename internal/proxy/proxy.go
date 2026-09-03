package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// proxy.go is chicco's inbound HTTP surface: the routing table and the handler
// for each endpoint. Everything a handler needs done is done elsewhere — this
// file only reads a request and writes a reply.

// Handler - Returns the HTTP handler: /v1/chat/completions is rotated across
// providers; /v1/messages is the same rotation in Anthropic's wire format;
// /v1/models lists available virtual models; /health is a liveness probe;
// /v1/status exposes a JSON snapshot for the web dashboard; /dashboard serves
// the live HTML dashboard page. logs may be nil (e.g. a caller with no use for
// log history) — the status endpoint returns an empty log array then.
func Handler(r *Rotator, logs *logBuffer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", r.handleHealth)
	mux.HandleFunc("/v1/models", r.handleModels)
	mux.HandleFunc("/v1/chat/completions", r.handleChat)
	mux.HandleFunc("/v1/embeddings", r.handleEmbeddings)
	mux.HandleFunc("/v1/messages", r.handleMessages)
	mux.HandleFunc("/v1/status", r.handleStatus(logs))
	mux.HandleFunc("/dashboard", handleDashboard)
	return r.withAuth(mux)
}

// healthStr - Renders a Health for the JSON APIs (/health, /v1/status) and the
// dashboard, which colour on these four strings.
func healthStr(h Health) string {
	switch h {
	case HealthOK:
		return "ok"
	case HealthAuth:
		return "auth"
	case HealthDown:
		return "down"
	default:
		return "unknown"
	}
}

// handleHealth - Serves GET /health. The status stays 200 whatever the
// providers are doing — the Helm chart points both the liveness AND the
// readiness probe here, so failing it on a provider outage would restart a
// proxy that is running perfectly well. The body is what changed: an empty 200
// said nothing about whether anything could actually serve a request, so a
// provider whose CLI isn't installed or whose key is rejected looked exactly as
// healthy as a working one. "degraded" means every provider is greyed out or in
// cooldown right now.
func (r *Rotator) handleHealth(w http.ResponseWriter, _ *http.Request) {
	providers := map[string]string{}
	ready := 0
	for _, s := range r.Snapshot() {
		if s.Inactive {
			continue
		}
		state := healthStr(s.Health)
		if s.CooldownLeft > 0 {
			state = "cooldown"
		}
		providers[s.Name] = state
		if state == "ok" || state == "unknown" { // unknown = not probed yet, still routable
			ready++
		}
	}
	status := "ok"
	if ready == 0 {
		status = "degraded"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "providers": providers})
}

// handleStatus - Returns a handler that serves GET /v1/status as JSON
// containing the current provider snapshot and the most recent log lines. It is
// the data source polled by the web dashboard.
func (r *Rotator) handleStatus(logs *logBuffer) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		type modelStatJSON struct {
			Name          string `json:"name"`
			Tokens        int64  `json:"tokens"`
			Requests      int    `json:"requests"`
			Quota         int64  `json:"quota"` // per-model quota (0 = use the provider's)
			QuotaIsTokens bool   `json:"quota_is_tokens"`
			UsedTokens    int64  `json:"used_tokens"` // only meaningful when Quota > 0
		}
		type providerStatJSON struct {
			Name          string          `json:"name"`
			Kind          string          `json:"kind"`
			Models        []modelStatJSON `json:"models"`
			Quota         int64           `json:"quota"`
			QuotaIsTokens bool            `json:"quota_is_tokens"`
			QuotaWindow   string          `json:"quota_window"`
			UsedTokens    int64           `json:"used_tokens"`
			Requests      int             `json:"requests"`
			CooldownSecs  float64         `json:"cooldown_secs"`
			CooldownKind  string          `json:"cooldown_kind"`
			Health        string          `json:"health"` // "ok" | "auth" | "down" | "unknown"
			Inactive      bool            `json:"inactive"`
		}

		stats := r.Snapshot()
		providers := make([]providerStatJSON, len(stats))
		for i, s := range stats {
			ms := make([]modelStatJSON, len(s.Models))
			for j, m := range s.Models {
				ms[j] = modelStatJSON{
					Name:          m.Name,
					Tokens:        m.Tokens,
					Requests:      m.Requests,
					Quota:         m.Quota,
					QuotaIsTokens: m.QuotaIsTokens,
					UsedTokens:    m.UsedTokens,
				}
			}
			providers[i] = providerStatJSON{
				Name:          s.Name,
				Kind:          s.Kind,
				Models:        ms,
				Quota:         s.Quota,
				QuotaIsTokens: s.QuotaIsTokens,
				QuotaWindow:   s.QuotaWindow,
				UsedTokens:    s.UsedTokens,
				Requests:      s.Requests,
				CooldownSecs:  s.CooldownLeft.Seconds(),
				CooldownKind:  s.CooldownKind,
				Health:        healthStr(s.Health),
				Inactive:      s.Inactive,
			}
		}

		var logLines []string
		if logs != nil {
			logLines = logs.tail(100)
		}
		if logLines == nil {
			logLines = []string{}
		}

		reqToday, tokToday, activeN := r.DailyTotals()

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"providers":        providers,
			"logs":             logLines,
			"requests_today":   reqToday,
			"tokens_today":     tokToday,
			"active_providers": activeN,
			// Session spend. Carries its own currency label and its unpriced
			// counts, so a dashboard cannot render the total as the whole bill
			// when part of the traffic could not be priced.
			"cost": r.CostSummary(),
		})
	}
}

// handleModels - Serves GET /v1/models in the OpenAI format: an object list of
// model descriptors. It includes one entry per virtual model defined in the
// routing table plus the catch-all "chicco:auto" that rotates across
// everything.
func (r *Rotator) handleModels(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ids := r.VirtualModelIDs()
	now := time.Now().Unix()
	type modelObj struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]modelObj, 0, len(ids)+1)
	// chicco:auto first — always present, routes across all active providers.
	data = append(data, modelObj{ID: "chicco:auto", Object: "model", Created: now, OwnedBy: "chicco"})
	for _, id := range ids {
		data = append(data, modelObj{ID: id, Object: "model", Created: now, OwnedBy: "chicco"})
	}
	// Aliases are listed too: a caller that cannot see them has to be told out
	// of band what names exist, which is the coupling aliases exist to remove.
	aliases := r.Aliases()
	names := make([]string, 0, len(aliases))
	for name := range aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data = append(data, modelObj{ID: name, Object: "model", Created: now, OwnedBy: "chicco (alias for " + aliases[name] + ")"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// readPostedJSON - Reads and decodes the JSON body of an OpenAI-shaped POST,
// writing the method/read/parse error itself and reporting ok=false when it
// did. Shared by handleChat and handleEmbeddings so both reject a GET and a
// malformed body with the same status and the same error envelope — a caller
// cannot tell which endpoint it hit from the failure.
func readPostedJSON(w http.ResponseWriter, req *http.Request) (map[string]any, bool) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil, false
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "chicco: read body: "+err.Error())
		return nil, false
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "chicco: invalid JSON body: "+err.Error())
		return nil, false
	}
	return payload, true
}

// handleChat - Forwards one chat-completion request, overriding the model with
// the rotation's pick and retrying the next provider on any
// quota/auth/transient failure until one answers (its response is streamed
// straight back) or all are exhausted. The rotation only fails over on the
// upstream's initial status, which is where quota/auth errors surface — once a
// 2xx body starts streaming to the client there is no rewinding it.
func (r *Rotator) handleChat(w http.ResponseWriter, req *http.Request) {
	payload, ok := readPostedJSON(w, req)
	if !ok {
		return
	}

	// Ask the upstream to append a final usage chunk so we can count tokens for
	// the dashboard. Harmless to providers that don't support it (extra field),
	// and to the caller (the chunk has empty choices, which clients ignore).
	if s, _ := payload["stream"].(bool); s {
		if _, ok := payload["stream_options"]; !ok {
			payload["stream_options"] = map[string]any{"include_usage": true}
		}
	}

	requestedModel, _ := payload["model"].(string)
	res, err := r.dispatch(req.Context(), requestedModel, payload, "/chat/completions")
	if err != nil {
		setRetryAfter(w, err)
		writeError(w, dispatchStatus(err), err.Error())
		return
	}
	usage := stream(w, res.up)
	r.recordUsage(res.provider, res.model, usage.Total)
	log.Printf("chicco: %s (%s) served %d tokens%s",
		res.provider, res.model, usage.Total, r.costNote(res.provider, res.model, usage))
}

// handleEmbeddings - Forwards one embeddings request the same way handleChat
// forwards a chat completion — rotation, cooldown and quota bookkeeping all go
// through the shared dispatch(). Embeddings responses are a single JSON object,
// never streamed, so unlike handleChat this reads the upstream body fully and
// relays it verbatim.
func (r *Rotator) handleEmbeddings(w http.ResponseWriter, req *http.Request) {
	payload, ok := readPostedJSON(w, req)
	if !ok {
		return
	}

	requestedModel, _ := payload["model"].(string)
	res, err := r.dispatch(req.Context(), requestedModel, payload, "/embeddings")
	if err != nil {
		setRetryAfter(w, err)
		writeError(w, dispatchStatus(err), err.Error())
		return
	}
	defer res.up.body.Close()
	respBody, err := io.ReadAll(res.up.body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "chicco: read upstream response: "+err.Error())
		return
	}
	var parsed struct {
		Usage struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(respBody, &parsed)
	r.recordUsage(res.provider, res.model, parsed.Usage.TotalTokens)
	log.Printf("chicco: %s (%s) served embeddings, %d tokens%s", res.provider, res.model,
		parsed.Usage.TotalTokens,
		r.costNote(res.provider, res.model, Usage{Total: parsed.Usage.TotalTokens, Prompt: parsed.Usage.TotalTokens}))

	contentType := res.up.contentType
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(res.up.status)
	_, _ = w.Write(respBody)
}

// setRetryAfter - Advertises when routing could next succeed, for a dispatch
// failure that carries a cooldown. Both the OpenAI and Anthropic SDKs honour
// Retry-After natively, so without it a client backs off on its own schedule
// and spends its whole retry budget inside a cooldown that outlasts it. Called
// on the exhaustion path in either wire format (writeError and
// writeAnthropicError both write w.Header() before the status line), never on
// the 4xx paths, where an unchanged request fails again however long you wait.
// RFC 9110 wants whole delta-seconds; round up and never emit 0, which reads as
// "retry now" and turns a back-off into a hot loop.
func setRetryAfter(w http.ResponseWriter, err error) {
	var de *dispatchError
	if !errors.As(err, &de) || de.retryAfter <= 0 {
		return
	}
	secs := (de.retryAfter + time.Second - 1) / time.Second
	w.Header().Set("Retry-After", strconv.FormatInt(int64(secs), 10))
}

// writeError - Replies with an OpenAI-style error envelope so a client parsing
// the standard shape surfaces a useful message.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "chicco_error"},
	})
}
