package proxy

import (
	"fmt"
	"sort"
	"sync"
)

// What a route costs, from prices declared in chicco.yaml.
//
// Prices come from config rather than from the providers: there is no common
// API for them, they change rarely, and a proxy that phoned home for a price
// list would have a new failure mode on the request path in exchange for a
// number nobody reads in the moment. Config is the honest place for them, and
// it is also the only place that knows what a free tier costs — which for most
// of chicco's providers is nothing, and that is a real answer rather than a
// missing one.
//
// The one rule this file exists to enforce: **a model with no configured price
// is unpriced, not free.** Every total says how many requests it could not
// price, because a cost report that quietly omits half the traffic is worse
// than no cost report.

// Price is what one model costs per million tokens, input and output billed
// separately — everywhere, and often by a factor of three or four, so a cost
// computed from a total token count is wrong by whatever shape the request
// happened to have.
//
// Per million rather than per token because that is the unit every published
// price list uses, and a config full of 0.00000015 invites a typo nobody spots.
type Price struct {
	InputPerMillion  float64 `yaml:"input_per_million"`
	OutputPerMillion float64 `yaml:"output_per_million"`
}

// Pricing is the config block: a currency and a price per model id.
type Pricing struct {
	// Currency is a label, not a conversion. chicco does no FX; it only
	// reports the unit the prices were written in, so a report cannot be read
	// as dollars when it was written in euros.
	Currency string           `yaml:"currency"`
	Models   map[string]Price `yaml:"models"`
}

// costTracker accumulates spend per provider and per model for the session.
type costTracker struct {
	mu       sync.Mutex
	currency string
	prices   map[string]Price
	byModel  map[string]float64
	byProv   map[string]float64
	// unpriced counts requests whose model had no price, per model. Kept
	// separately and reported: folding them in as zero would understate the
	// bill while looking precise.
	unpriced map[string]int
	requests int
}

func newCostTracker(p Pricing) *costTracker {
	prices := make(map[string]Price, len(p.Models))
	for k, v := range p.Models {
		prices[k] = v
	}
	currency := p.Currency
	if currency == "" && len(prices) > 0 {
		// A price list with no currency is ambiguous in a way that matters,
		// so it is labelled rather than assumed.
		currency = "unspecified"
	}
	return &costTracker{
		currency: currency,
		prices:   prices,
		byModel:  map[string]float64{},
		byProv:   map[string]float64{},
		unpriced: map[string]int{},
	}
}

// cost prices one request. ok is false when the model has no configured price.
func (c *costTracker) cost(model string, u Usage) (float64, bool) {
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	price, ok := c.prices[model]
	c.mu.Unlock()
	if !ok {
		return 0, false
	}
	// If a provider reported only a total, the split is unknown; charging it
	// all at the input rate would understate, so the output rate is used —
	// wrong in the safe direction for a number people budget against.
	in, out := u.Prompt, u.Completion
	if in == 0 && out == 0 {
		out = u.Total
	}
	return float64(in)/1e6*price.InputPerMillion + float64(out)/1e6*price.OutputPerMillion, true
}

// record prices a request and adds it to the session totals.
func (c *costTracker) record(provider, model string, u Usage) (float64, bool) {
	if c == nil {
		return 0, false
	}
	usd, ok := c.cost(model, u)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	if !ok {
		c.unpriced[model]++
		return 0, false
	}
	c.byModel[model] += usd
	c.byProv[provider] += usd
	return usd, true
}

// CostSummary is the session's spend, for the UI and the status endpoint.
type CostSummary struct {
	Currency   string             `json:"currency"`
	Total      float64            `json:"total"`
	ByProvider map[string]float64 `json:"by_provider"`
	ByModel    map[string]float64 `json:"by_model"`
	// Unpriced is how many requests each unpriced model served. Its presence
	// is what stops Total being read as the whole bill.
	Unpriced map[string]int `json:"unpriced,omitempty"`
	Requests int            `json:"requests"`
}

func (c *costTracker) summary() CostSummary {
	if c == nil {
		return CostSummary{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := CostSummary{
		Currency:   c.currency,
		ByProvider: make(map[string]float64, len(c.byProv)),
		ByModel:    make(map[string]float64, len(c.byModel)),
		Requests:   c.requests,
	}
	for k, v := range c.byProv {
		s.ByProvider[k] = round4(v)
		s.Total += v
	}
	for k, v := range c.byModel {
		s.ByModel[k] = round4(v)
	}
	if len(c.unpriced) > 0 {
		s.Unpriced = make(map[string]int, len(c.unpriced))
		for k, v := range c.unpriced {
			s.Unpriced[k] = v
		}
	}
	s.Total = round4(s.Total)
	return s
}

func round4(v float64) float64 {
	return float64(int64(v*10000+0.5)) / 10000
}

// UnpricedModels lists models seen without a price, sorted — the line a
// dashboard shows so an incomplete total is visibly incomplete.
func (c *costTracker) UnpricedModels() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.unpriced))
	for k := range c.unpriced {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// costNote is the suffix the per-request log line carries. Empty when no
// pricing is configured at all, so a user who has not opted in sees no change;
// "unpriced" when prices exist but not for this model, which is the case worth
// noticing.
func (r *Rotator) costNote(provider, model string, u Usage) string {
	if r == nil || r.costs == nil || len(r.costs.prices) == 0 {
		return ""
	}
	usd, ok := r.costs.record(provider, model, u)
	if !ok {
		return " (unpriced)"
	}
	return fmt.Sprintf(" (%.4f %s)", usd, r.costs.currency)
}

// CostSummary exposes the session totals.
func (r *Rotator) CostSummary() CostSummary { return r.costs.summary() }
