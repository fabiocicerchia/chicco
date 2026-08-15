package proxy

import (
	"math"
	"testing"
)

func pricing() Pricing {
	return Pricing{
		Currency: "USD",
		Models: map[string]Price{
			// Input and output deliberately differ by 4x, which is what makes
			// pricing off a total token count wrong.
			"gpt-x": {InputPerMillion: 0.50, OutputPerMillion: 2.00},
			"free":  {InputPerMillion: 0, OutputPerMillion: 0},
		},
	}
}

func closeTo(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCostUsesTheInputOutputSplit(t *testing.T) {
	c := newCostTracker(pricing())
	// 1M in at 0.50 + 0.5M out at 2.00 = 0.50 + 1.00
	usd, ok := c.cost("gpt-x", Usage{Prompt: 1_000_000, Completion: 500_000, Total: 1_500_000})
	if !ok {
		t.Fatal("priced model reported unpriced")
	}
	closeTo(t, usd, 1.50)
}

func TestATotalOnlyResponseIsPricedAtTheOutputRate(t *testing.T) {
	// Some providers report only total_tokens. Charging it all at the input
	// rate would understate the bill; the output rate errs the safe way.
	c := newCostTracker(pricing())
	usd, _ := c.cost("gpt-x", Usage{Total: 1_000_000})
	closeTo(t, usd, 2.00)
}

func TestUnpricedIsNotZero(t *testing.T) {
	c := newCostTracker(pricing())
	if _, ok := c.cost("some-model-nobody-priced", Usage{Total: 1_000_000}); ok {
		t.Fatal("an unconfigured model must not report a price")
	}

	c.record("groq", "some-model-nobody-priced", Usage{Total: 1_000_000})
	s := c.summary()
	if s.Total != 0 {
		t.Errorf("total = %v, want 0 — an unpriced request contributes nothing", s.Total)
	}
	if s.Unpriced["some-model-nobody-priced"] != 1 {
		t.Errorf("unpriced = %v, want the request counted", s.Unpriced)
	}
	if _, listed := s.ByModel["some-model-nobody-priced"]; listed {
		t.Error("an unpriced model must not appear in ByModel at all")
	}
}

func TestAConfiguredZeroPriceIsFreeNotUnpriced(t *testing.T) {
	// The distinction that matters for chicco specifically: most of its
	// providers are free tiers, and "this costs nothing" is a real answer.
	c := newCostTracker(pricing())
	usd, ok := c.record("groq", "free", Usage{Prompt: 1000, Completion: 1000})
	if !ok {
		t.Fatal("a configured zero price is a price")
	}
	closeTo(t, usd, 0)
	if s := c.summary(); len(s.Unpriced) != 0 {
		t.Errorf("unpriced = %v, want empty", s.Unpriced)
	}
}

func TestSessionTotalsSplitByProviderAndModel(t *testing.T) {
	c := newCostTracker(pricing())
	c.record("groq", "gpt-x", Usage{Prompt: 1_000_000, Completion: 0})    // 0.50
	c.record("cerebras", "gpt-x", Usage{Prompt: 0, Completion: 500_000})  // 1.00
	c.record("groq", "free", Usage{Prompt: 1_000_000, Completion: 1_000}) // 0
	c.record("groq", "mystery", Usage{Total: 10})                         // unpriced

	s := c.summary()
	closeTo(t, s.Total, 1.50)
	closeTo(t, s.ByProvider["groq"], 0.50)
	closeTo(t, s.ByProvider["cerebras"], 1.00)
	closeTo(t, s.ByModel["gpt-x"], 1.50)
	if s.Requests != 4 {
		t.Errorf("requests = %d, want 4 including the unpriced one", s.Requests)
	}
	if s.Currency != "USD" {
		t.Errorf("currency = %q", s.Currency)
	}
	if s.Unpriced["mystery"] != 1 {
		t.Errorf("unpriced = %v", s.Unpriced)
	}
}

func TestCurrencyIsLabelledNotAssumed(t *testing.T) {
	// Prices with no currency are ambiguous in a way that matters, and chicco
	// does no conversion, so the ambiguity is reported rather than guessed.
	c := newCostTracker(Pricing{Models: map[string]Price{"m": {InputPerMillion: 1}}})
	if c.summary().Currency != "unspecified" {
		t.Errorf("currency = %q, want unspecified", c.summary().Currency)
	}
	// No prices at all: nothing to label.
	if newCostTracker(Pricing{}).summary().Currency != "" {
		t.Error("an empty price list should not invent a currency")
	}
}

func TestCostNoteOnlyAppearsWhenPricingIsConfigured(t *testing.T) {
	r := NewRotator(nil, nil)
	// Default rotator: no pricing, so the log line is unchanged for anyone who
	// has not opted in.
	if note := r.costNote("groq", "gpt-x", Usage{Total: 100}); note != "" {
		t.Errorf("note = %q, want empty with no pricing configured", note)
	}

	r.costs = newCostTracker(pricing())
	if note := r.costNote("groq", "gpt-x", Usage{Prompt: 1_000_000}); note != " (0.5000 USD)" {
		t.Errorf("note = %q", note)
	}
	if note := r.costNote("groq", "unknown", Usage{Total: 5}); note != " (unpriced)" {
		t.Errorf("note = %q, want it to say unpriced", note)
	}
}

func TestUsageSplitReadsBothVocabularies(t *testing.T) {
	openai := usageSplit([]byte(`data: {"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	if openai != (Usage{Total: 15, Prompt: 10, Completion: 5}) {
		t.Errorf("openai = %+v", openai)
	}
	// A provider answering in Anthropic's shape.
	anthropic := usageSplit([]byte(`{"usage":{"input_tokens":10,"output_tokens":5}}`))
	if anthropic != (Usage{Total: 15, Prompt: 10, Completion: 5}) {
		t.Errorf("anthropic = %+v", anthropic)
	}
	// Total present, split absent: total is kept and the split stays zero, so
	// the pricing path can tell the difference.
	only := usageSplit([]byte(`{"usage":{"total_tokens":42}}`))
	if only != (Usage{Total: 42}) {
		t.Errorf("total-only = %+v", only)
	}
	if got := usageSplit([]byte("not json")); got != (Usage{}) {
		t.Errorf("junk = %+v", got)
	}
}

func TestUsageTokensStillWorksForCallersThatOnlyWantTheTotal(t *testing.T) {
	if got := usageTokens([]byte(`{"usage":{"total_tokens":7}}`)); got != 7 {
		t.Errorf("got %d, want 7", got)
	}
}
