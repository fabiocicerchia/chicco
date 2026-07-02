package proxy

import (
	"strings"
	"testing"
)

// hasProblem reports whether any validation message contains sub.
func hasProblem(problems []string, sub string) bool {
	for _, p := range problems {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

// TestValidateCatchesMistakes checks the static config check flags the common
// misconfigurations `chicco -check` is meant to catch.
func TestValidateCatchesMistakes(t *testing.T) {
	cfg := Config{Providers: []Provider{
		{Name: "http-ok", BaseURL: "http://x", APIKey: "k", Models: []string{"m"}},
		{Name: "cli-nocmd", Kind: "cli", Models: []string{"m"}},                    // missing command
		{Name: "http-nourl", APIKey: "k", Models: []string{"m"}},                   // missing base_url
		{Name: "http-ok", BaseURL: "http://y", APIKey: "k", Models: []string{"m"}}, // dup name
		{Name: "bad-kind", BaseURL: "http://x", APIKey: "k", Models: []string{"m"}, Kind: "grpc"},
		{Name: "bad-json", BaseURL: "http://x", APIKey: "k", Models: []string{"m"}, Output: "json"}, // no result_path
	}}
	problems := cfg.Validate()

	for _, want := range []string{
		"cli-nocmd: kind: cli needs a command",
		"http-nourl: missing base_url",
		"http-ok: duplicate provider name",
		"bad-kind: unknown kind",
		"bad-json: output: json needs a result_path",
	} {
		if !hasProblem(problems, want) {
			t.Errorf("Validate() missing expected problem %q; got %v", want, problems)
		}
	}
}

// TestValidateWarnsInactive flags providers that would be silently skipped, marked
// as warnings (not hard errors).
func TestValidateWarnsInactive(t *testing.T) {
	cfg := Config{Providers: []Provider{
		{Name: "nokey", BaseURL: "http://x", Models: []string{"m"}}, // http, no key
		{Name: "nomodel", BaseURL: "http://x", APIKey: "k"},         // no models
	}}
	problems := cfg.Validate()
	if !hasProblem(problems, "warning: nokey") || !hasProblem(problems, "warning: nomodel") {
		t.Errorf("expected inactive-provider warnings, got %v", problems)
	}
	if !hasProblem(problems, "warning: no active providers") {
		t.Errorf("expected a no-active-providers warning, got %v", problems)
	}
}

// TestValidateCleanConfig passes a well-formed config with no problems.
func TestValidateCleanConfig(t *testing.T) {
	cfg := Config{Providers: []Provider{
		{Name: "groq", BaseURL: "http://x", APIKey: "k", Models: []string{"m"}, Quota: Quota{TPD: 1000}},
		{Name: "claude", Kind: "cli", Command: "claude", Models: []string{"sonnet"}, Output: "json", ResultPath: "result"},
	}}
	if problems := cfg.Validate(); len(problems) != 0 {
		t.Errorf("clean config reported problems: %v", problems)
	}
}

// TestValidateModelStrategy checks strategy validation is per virtual model: an
// unknown strategy is flagged by name.
func TestValidateModelStrategy(t *testing.T) {
	cfg := Config{
		Providers: []Provider{{Name: "a", BaseURL: "http://x", APIKey: "k", Models: []string{"m"}}},
		Models: []Model{
			{ID: "bad", Strategy: "fifo", Backends: []Backend{{Provider: "a", Model: "m"}}},
		},
	}
	problems := cfg.Validate()
	if !hasProblem(problems, `model bad: unknown strategy "fifo"`) {
		t.Errorf("expected unknown-strategy problem for model bad, got %v", problems)
	}
}
