package proxy

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// config.go is the shape of chicco.yaml: the types, their YAML decoding and the
// quota arithmetic derived from them.

// DefaultAddr is the address chicco listens on when chicco.yaml sets no addr.
// Loopback, not ":41986": the default config has no api_key, so a wildcard bind
// would hand every provider key to anything that can reach the port. Set addr
// explicitly to expose it, and set api_key at the same time — RequireAuthOnBind
// refuses the combination.
const DefaultAddr = "127.0.0.1:41986"

// Config is chicco.yaml.
type Config struct {
	Addr string `yaml:"addr"`
	// APIKey, when set, is a shared secret guarding chicco's own inbound endpoints:
	// callers must present it as `Authorization: Bearer <key>`. Empty (the default)
	// leaves chicco open, which is why the default addr is loopback-only; binding
	// anywhere else without a key is refused at startup. ${VAR} is expanded from
	// the environment.
	APIKey    string     `yaml:"api_key"`
	Providers []Provider // populated by UnmarshalYAML from either a list or a map
	Models    []Model    `yaml:"models"` // virtual model routing table (see Rotator.activeForModel)
	// Quota, when set, caps usage across every provider combined (same fields as a
	// provider's own quota:) — the pooled-total ceiling chicco doesn't otherwise
	// have, since its whole point is to drain one provider's quota and fall
	// through to the next. Zero value (the default) means no aggregate cap.
	Quota Quota `yaml:"quota"`
	// Aliases map a caller-facing name onto a virtual model id, so a client can
	// ask for "fast" and the routing table decides what that means today.
	// Changing a backend then means editing chicco.yaml, not every caller.
	Aliases map[string]string `yaml:"aliases"`
	// Metrics, when enabled, serves a Prometheus exposition on its own listener.
	// Off by default and on a separate address on purpose: the proxy port is
	// what an agent runner points at, and a scrape endpoint is a different
	// audience with a different exposure.
	Metrics MetricsConfig `yaml:"metrics"`
	// Pricing turns the token counters into money. Optional: with no prices
	// configured, every request reports as unpriced rather than as free.
	Pricing Pricing `yaml:"pricing"`
	// Alerts warns when a provider approaches its quota, instead of the
	// exhaustion being discovered when requests start failing.
	Alerts AlertConfig `yaml:"alerts"`
}

// MetricsConfig configures the Prometheus endpoint. Addr defaults to
// 127.0.0.1:41987 — one above the proxy's own port, and loopback, because the
// exposition has no auth of its own.
type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Addr    string `yaml:"addr"`
}

// DefaultMetricsAddr is used when metrics are enabled with no addr set.
const DefaultMetricsAddr = "127.0.0.1:41987"

// rawConfig is the intermediate shape used during YAML decoding. providers is
// kept as a raw yaml.Node so we can detect list vs. map and preserve order.
type rawConfig struct {
	Addr      string            `yaml:"addr"`
	APIKey    string            `yaml:"api_key"`
	Providers yaml.Node         `yaml:"providers"`
	Models    []Model           `yaml:"models"`
	Quota     Quota             `yaml:"quota"`
	Aliases   map[string]string `yaml:"aliases"`
	Metrics   MetricsConfig     `yaml:"metrics"`
	Pricing   Pricing           `yaml:"pricing"`
	Alerts    AlertConfig       `yaml:"alerts"`
}

// UnmarshalYAML - Lets Config accept providers as either a YAML sequence (list
// format, original chicco.yaml) or a YAML mapping (keyed format, where the map
// key becomes Provider.Name). Both produce the same []Provider slice in the
// same document order, so the rest of the code is format-agnostic.
func (c *Config) UnmarshalYAML(value *yaml.Node) error {
	var raw rawConfig
	if err := value.Decode(&raw); err != nil {
		return err
	}
	c.Addr = raw.Addr
	c.APIKey = raw.APIKey
	c.Models = raw.Models
	c.Quota = raw.Quota
	c.Aliases = raw.Aliases
	c.Metrics = raw.Metrics
	if c.Metrics.Enabled && c.Metrics.Addr == "" {
		c.Metrics.Addr = DefaultMetricsAddr
	}
	c.Pricing = raw.Pricing
	c.Alerts = raw.Alerts

	providers, err := decodeProviders(&raw.Providers)
	if err != nil {
		return err
	}
	c.Providers = providers
	return nil
}

// decodeProviders - Decodes the providers: node in either supported format,
// keeping document order in both.
func decodeProviders(n *yaml.Node) ([]Provider, error) {
	// Unwrap a document node if present.
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}

	switch n.Kind {
	case yaml.Kind(0): // absent / null — zero providers is fine
		return nil, nil

	case yaml.SequenceNode:
		// Original list format:
		//   providers:
		//     - name: groq
		//       base_url: …
		var out []Provider
		if err := n.Decode(&out); err != nil {
			return nil, fmt.Errorf("providers (list): %w", err)
		}
		return out, nil

	case yaml.MappingNode:
		return decodeProviderMap(n)

	default:
		return nil, fmt.Errorf("providers: expected a list or map, got YAML kind %v", n.Kind)
	}
}

// decodeProviderMap - Decodes the keyed map format:
//
//	providers:
//	  groq:
//	    base_url: …
//
// The map key becomes Provider.Name unless the entry sets one itself.
func decodeProviderMap(n *yaml.Node) ([]Provider, error) {
	// Mapping nodes are pairs: [key0, val0, key1, val1, …].
	if len(n.Content)%2 != 0 {
		return nil, fmt.Errorf("providers: malformed mapping node")
	}
	out := make([]Provider, 0, len(n.Content)/2)
	for i := 0; i < len(n.Content); i += 2 {
		nameNode := n.Content[i]
		valNode := n.Content[i+1]
		var p Provider
		if err := valNode.Decode(&p); err != nil {
			return nil, fmt.Errorf("provider %q: %w", nameNode.Value, err)
		}
		// Use the map key as the name when the entry itself has no name: field.
		if p.Name == "" {
			p.Name = nameNode.Value
		}
		out = append(out, p)
	}
	return out, nil
}

// Quota holds the client-side rate-limit caps for a provider. All fields are
// optional; zero means no limit for that dimension.
// RPM/RPH/RPD: max requests per minute / hour / day.
// TPM/TPH/TPD: max tokens per minute / hour / day.
// The tightest daily limit also drives the dashboard usage bar.
type Quota struct {
	RPM int   `yaml:"rpm"` // requests per minute
	RPH int   `yaml:"rph"` // requests per hour
	RPD int   `yaml:"rpd"` // requests per day
	TPM int64 `yaml:"tpm"` // tokens per minute
	TPH int64 `yaml:"tph"` // tokens per hour
	TPD int64 `yaml:"tpd"` // tokens per day
}

// Provider is one upstream OpenAI-compatible endpoint. APIKey is sent as a Bearer
// token (it is run through os.ExpandEnv on load, so `${GROQ_API_KEY}` works);
// Models is round-robined per provider. A provider with an empty key or no models
// is inactive and skipped.
type Provider struct {
	Name    string   `yaml:"name"`
	BaseURL string   `yaml:"base_url"`
	APIKey  string   `yaml:"api_key"`
	Models  []string `yaml:"models"`

	// Quota holds client-side rate limits enforced before forwarding, so a
	// provider is put in cooldown the moment a limit would be breached rather than
	// waiting for the upstream to 429.
	Quota Quota `yaml:"quota"`

	// CLI provider fields (kind: cli). Instead of an HTTP call, chicco runs Command
	// with Args (templated with {{model}}/{{system}}/{{user}}/{{prompt}}/{{output_file}}),
	// buffers the completion, and synthesizes the OpenAI SSE the caller expects.
	// Weight biases the "weighted" load-balancing strategy: a provider with weight 3
	// is picked roughly 3× as often as one with weight 1. Unset/0 counts as 1.
	// Ignored by the other strategies.
	Weight int `yaml:"weight"`

	Kind          string   `yaml:"kind"`              // "" / "http" (default) | "cli"
	Command       string   `yaml:"command"`           // tool to run, e.g. "claude"
	Args          []string `yaml:"args"`              // templated argv
	PromptStdin   bool     `yaml:"prompt_stdin"`      // pipe {{prompt}} on stdin instead of an arg
	OutputFile    bool     `yaml:"output_file"`       // read completion from the {{output_file}} temp path
	Output        string   `yaml:"output"`            // "text" (default) | "json"
	ResultPath    string   `yaml:"result_path"`       // dotted path to text when Output=="json"
	TokensPath    string   `yaml:"tokens_path"`       // optional dotted path to the completion token count
	InTokensPath  string   `yaml:"input_tokens_path"` // optional dotted path to the prompt token count (else estimated)
	ErrorPath     string   `yaml:"error_path"`        // optional dotted path; if truthy the call failed (fail over)
	StripANSI     bool     `yaml:"strip_ansi"`        // strip ANSI escapes from output (kiro)
	HealthCommand []string `yaml:"health_command"`    // run for health; exit 0 (and HealthExpect, if set) = healthy
	HealthExpect  string   `yaml:"health_expect"`     // require this substring in HealthCommand output, else HealthAuth (logged out)
	Credential    string   `yaml:"credential"`        // optional file to stat for health (use ${HOME}/…)
	TimeoutSecs   int      `yaml:"timeout_seconds"`   // CLI run timeout (default 120)
}

// effectiveQuota - Derives the dashboard bar parameters from the quota fields.
// It returns the quota value, whether it is token-based (true) or request-based
// (false), and the window string for eventLog.windowTotals. Priority: day >
// hour > minute (largest window first, so the bar tracks the most meaningful
// hard cap). Returns quota=0 when no limits are configured.
func (p Provider) effectiveQuota() (quota int64, isTokens bool, window string) {
	return quotaBar(p.Quota)
}

// quotaBar - Resolves one Quota into the dashboard bar parameters. It is the
// single copy of the day > hour > minute priority: Provider and Backend both
// read it, and a window added to Quota that only one of them learned about
// would draw two different bars for the same limit.
func quotaBar(q Quota) (quota int64, isTokens bool, window string) {
	switch {
	case q.TPD > 0:
		return q.TPD, true, "daily"
	case q.RPD > 0:
		return int64(q.RPD), false, "daily"
	case q.TPH > 0:
		return q.TPH, true, "hourly"
	case q.RPH > 0:
		return int64(q.RPH), false, "hourly"
	case q.TPM > 0:
		return q.TPM, true, "minutely"
	case q.RPM > 0:
		return int64(q.RPM), false, "minutely"
	default:
		return 0, false, "none"
	}
}

// Model is a named virtual model that routes across one or more provider backends.
type Model struct {
	ID string `yaml:"id"`
	// Strategy is the load-balancing order across this model's backends: "" /
	// "order" (config order — drain the top backend, then fall through; the
	// default), "round_robin" (rotate the starting backend each request),
	// "random", or "weighted" (biased by each backend provider's weight). See
	// Rotator.order. Requests that don't match a virtual model (chicco:auto or
	// an unknown model) always use "order" across all active providers.
	Strategy string    `yaml:"strategy"`
	Backends []Backend `yaml:"backends"`
}

// Backend is one entry in a Model's backend list.
// Quota overrides the parent provider's quota for this specific model when set.
// This lets you enforce per-model limits (e.g. Groq's per-model daily caps) that
// differ from the provider's aggregate quota. Weight likewise overrides the
// provider's "weighted" strategy bias for this specific model, since the same
// provider may back several virtual models with a different priority in each.
type Backend struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	Quota    *Quota `yaml:"quota,omitempty"`  // nil → inherit provider quota
	Weight   *int   `yaml:"weight,omitempty"` // nil → inherit provider weight
}

// effectiveWeight - Returns the weight for this backend: its own override when
// configured (nil → inherit), otherwise the given provider fallback.
func (b Backend) effectiveWeight(fallback int) int {
	if b.Weight != nil {
		return *b.Weight
	}
	return fallback
}

// effectiveQuota - Returns the quota for this backend: the backend's own quota
// when configured, otherwise the given provider fallback. Same return semantics
// as Provider.effectiveQuota.
func (b Backend) effectiveQuota(fallback Quota) (quota int64, isTokens bool, window string) {
	q := fallback
	if b.Quota != nil {
		q = *b.Quota
	}
	return quotaBar(q)
}
