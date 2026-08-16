package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const aliasYAML = `
providers:
  groq:
    base_url: https://example.invalid
    api_key: k
    models: [llama-70b]
  cerebras:
    base_url: https://example.invalid
    api_key: k
    models: [llama-70b-cerebras]
models:
  - id: llama-3.3-70b
    strategy: order
    backends:
      - provider: cerebras
        model: llama-70b-cerebras
      - provider: groq
        model: llama-70b
aliases:
  fast: llama-3.3-70b
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chicco.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAliasResolvesToItsModel(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, aliasYAML))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	r := NewRotator(cfg.Providers, cfg.Models)
	r.aliases = cfg.Aliases

	// The alias must select the same providers, in the same order, as asking
	// for the model directly — that is the whole contract.
	viaAlias, sAlias := r.activeForModel("fast")
	direct, sDirect := r.activeForModel("llama-3.3-70b")
	if len(viaAlias) != len(direct) || sAlias != sDirect {
		t.Fatalf("alias gave %d providers/%q, direct gave %d/%q",
			len(viaAlias), sAlias, len(direct), sDirect)
	}
	for i := range direct {
		if viaAlias[i].Name != direct[i].Name {
			t.Errorf("provider %d: alias %s, direct %s", i, viaAlias[i].Name, direct[i].Name)
		}
	}
}

func TestAliasReusesTheExistingFailoverPath(t *testing.T) {
	// The issue's requirement: an alias maps onto an ordered backend list and
	// reuses the failover already there, rather than getting a second path.
	// What that means concretely is that each provider is narrowed to the
	// model THIS virtual model names for it, not the provider's whole list.
	cfg, _ := LoadConfig(writeConfig(t, aliasYAML))
	r := NewRotator(cfg.Providers, cfg.Models)
	r.aliases = cfg.Aliases

	got, strategy := r.activeForModel("fast")
	if strategy != "order" {
		t.Errorf("strategy = %q, want order", strategy)
	}
	want := map[string]string{"cerebras": "llama-70b-cerebras", "groq": "llama-70b"}
	if len(got) != len(want) {
		t.Fatalf("got %d providers, want %d", len(got), len(want))
	}
	for _, p := range got {
		if len(p.Models) != 1 || p.Models[0] != want[p.Name] {
			t.Errorf("%s models = %v, want [%s]", p.Name, p.Models, want[p.Name])
		}
	}
}

func TestAliasPointingAtNothingIsRefusedAtLoad(t *testing.T) {
	// Not at request time: an alias that silently falls through to full
	// rotation gives the caller an arbitrary routing policy with nothing in
	// the logs saying so, which is the failure this feature exists to remove.
	_, err := LoadConfig(writeConfig(t, aliasYAML+"  broken: no-such-model\n"))
	if err == nil {
		t.Fatal("want an error for an alias with no target model")
	}
	if !strings.Contains(err.Error(), "broken") || !strings.Contains(err.Error(), "no-such-model") {
		t.Errorf("error should name both the alias and its target: %v", err)
	}
}

func TestAliasWithAnEmptyTargetIsRefused(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, aliasYAML+"  empty: \"\"\n"))
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v, want it to name the alias", err)
	}
}

func TestAliasCannotShadowAModelID(t *testing.T) {
	// Otherwise routing depends on which lookup happens first.
	_, err := LoadConfig(writeConfig(t, aliasYAML+"  llama-3.3-70b: llama-3.3-70b\n"))
	if err == nil || !strings.Contains(err.Error(), "same name") {
		t.Errorf("err = %v, want a shadowing complaint", err)
	}
}

func TestUnknownModelStillFallsBackRatherThanFailing(t *testing.T) {
	// Unchanged behaviour for a plain unknown model: aliases tighten the
	// contract for names the config knows about, not for everything.
	cfg, _ := LoadConfig(writeConfig(t, aliasYAML))
	r := NewRotator(cfg.Providers, cfg.Models)
	r.aliases = cfg.Aliases
	got, strategy := r.activeForModel("something-nobody-configured")
	if len(got) == 0 || strategy != "order" {
		t.Errorf("got %d providers/%q, want the full rotation", len(got), strategy)
	}
}

func TestReloadPicksUpAliasChanges(t *testing.T) {
	cfg, _ := LoadConfig(writeConfig(t, aliasYAML))
	r := NewRotator(cfg.Providers, cfg.Models)
	r.aliases = cfg.Aliases

	next, err := LoadConfig(writeConfig(t, strings.Replace(
		aliasYAML, "fast: llama-3.3-70b", "quick: llama-3.3-70b", 1)))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	r.Reload(next)

	if _, ok := r.resolveAlias("fast"); ok {
		t.Error("the removed alias still resolves after a reload")
	}
	if target, ok := r.resolveAlias("quick"); !ok || target != "llama-3.3-70b" {
		t.Errorf("new alias = %q/%v after reload", target, ok)
	}
}

func TestAliasesAreListedOnTheModelsEndpoint(t *testing.T) {
	cfg, _ := LoadConfig(writeConfig(t, aliasYAML))
	r := NewRotator(cfg.Providers, cfg.Models)
	r.aliases = cfg.Aliases

	rec := httptest.NewRecorder()
	r.handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, m := range body.Data {
		if m.ID == "fast" {
			if !strings.Contains(m.OwnedBy, "llama-3.3-70b") {
				t.Errorf("alias listed without naming its target: %q", m.OwnedBy)
			}
			return
		}
	}
	t.Errorf("alias missing from /v1/models: %+v", body.Data)
}

func TestNoAliasesIsNotAnError(t *testing.T) {
	body := strings.Split(aliasYAML, "aliases:")[0]
	if _, err := LoadConfig(writeConfig(t, body)); err != nil {
		t.Errorf("a config with no aliases must load: %v", err)
	}
}
