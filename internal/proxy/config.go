package proxy

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

// DefaultAddr is the port chicco listens on when chicco.yaml sets no addr.
const DefaultAddr = ":41986"

// Config is chicco.yaml.
type Config struct {
	Addr string `yaml:"addr"`
	// APIKey, when set, is a shared secret guarding chicco's own inbound endpoints:
	// callers must present it as `Authorization: Bearer <key>`. Empty (the default)
	// leaves chicco open, which is fine bound to 127.0.0.1; set it when exposing
	// chicco beyond localhost. ${VAR} is expanded from the environment.
	APIKey    string     `yaml:"api_key"`
	Providers []Provider // populated by UnmarshalYAML from either a list or a map
	Models    []Model    `yaml:"models"` // forward-looking routing table (not yet wired up)
}

// rawConfig is the intermediate shape used during YAML decoding. providers is
// kept as a raw yaml.Node so we can detect list vs. map and preserve order.
type rawConfig struct {
	Addr      string    `yaml:"addr"`
	APIKey    string    `yaml:"api_key"`
	Providers yaml.Node `yaml:"providers"`
	Models    []Model   `yaml:"models"`
}

// UnmarshalYAML lets Config accept providers as either a YAML sequence (list
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

	n := &raw.Providers
	// Unwrap a document node if present.
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}

	switch n.Kind {
	case yaml.Kind(0): // absent / null — zero providers is fine
		// nothing to do

	case yaml.SequenceNode:
		// Original list format:
		//   providers:
		//     - name: groq
		//       base_url: …
		if err := n.Decode(&c.Providers); err != nil {
			return fmt.Errorf("providers (list): %w", err)
		}

	case yaml.MappingNode:
		// Keyed map format:
		//   providers:
		//     groq:
		//       base_url: …
		// Mapping nodes are pairs: [key0, val0, key1, val1, …].
		if len(n.Content)%2 != 0 {
			return fmt.Errorf("providers: malformed mapping node")
		}
		c.Providers = make([]Provider, 0, len(n.Content)/2)
		for i := 0; i < len(n.Content); i += 2 {
			nameNode := n.Content[i]
			valNode := n.Content[i+1]
			var p Provider
			if err := valNode.Decode(&p); err != nil {
				return fmt.Errorf("provider %q: %w", nameNode.Value, err)
			}
			// Use the map key as the name when the entry itself has no name: field.
			if p.Name == "" {
				p.Name = nameNode.Value
			}
			c.Providers = append(c.Providers, p)
		}

	default:
		return fmt.Errorf("providers: expected a list or map, got YAML kind %v", n.Kind)
	}

	return nil
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

	Kind          string   `yaml:"kind"`            // "" / "http" (default) | "cli"
	Command       string   `yaml:"command"`         // tool to run, e.g. "claude"
	Args          []string `yaml:"args"`            // templated argv
	PromptStdin   bool     `yaml:"prompt_stdin"`    // pipe {{prompt}} on stdin instead of an arg
	OutputFile    bool     `yaml:"output_file"`     // read completion from the {{output_file}} temp path
	Output        string   `yaml:"output"`          // "text" (default) | "json"
	ResultPath    string   `yaml:"result_path"`     // dotted path to text when Output=="json"
	TokensPath    string   `yaml:"tokens_path"`     // optional dotted path to a token count
	ErrorPath     string   `yaml:"error_path"`      // optional dotted path; if truthy the call failed (fail over)
	StripANSI     bool     `yaml:"strip_ansi"`      // strip ANSI escapes from output (kiro)
	HealthCommand []string `yaml:"health_command"`  // run for health; exit 0 (and HealthExpect, if set) = healthy
	HealthExpect  string   `yaml:"health_expect"`   // require this substring in HealthCommand output, else HealthAuth (logged out)
	Credential    string   `yaml:"credential"`      // optional file to stat for health (use ${HOME}/…)
	TimeoutSecs   int      `yaml:"timeout_seconds"` // CLI run timeout (default 120)
}

// effectiveQuota derives the dashboard bar parameters from the quota fields.
// It returns the quota value, whether it is token-based (true) or request-based
// (false), and the window string for eventLog.windowTotals.
// Priority: day > hour > minute (largest window first, so the bar tracks the
// most meaningful hard cap). Returns quota=0 when no limits are configured.
func (p Provider) effectiveQuota() (quota int64, isTokens bool, window string) {
	q := p.Quota
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
// differ from the provider's aggregate quota.
type Backend struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	Quota    *Quota `yaml:"quota,omitempty"` // nil → inherit provider quota
}

// effectiveQuota returns the quota for this backend: the backend's own quota
// when configured, otherwise the given provider fallback. Same return semantics
// as Provider.effectiveQuota.
func (b Backend) effectiveQuota(fallback Quota) (quota int64, isTokens bool, window string) {
	q := fallback
	if b.Quota != nil {
		q = *b.Quota
	}
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

// LoadConfig reads and parses chicco.yaml, defaulting the listen address and
// expanding ${VAR} references in each provider's api_key from the environment.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if c.Addr == "" {
		c.Addr = DefaultAddr
	}

	// If providers were declared without a models: list (map format), populate
	// Provider.Models from the models: routing table so the rest of the code
	// (which requires at least one model per active provider) works unchanged.
	resolveModels(&c)

	c.APIKey = os.ExpandEnv(c.APIKey)
	// Expand ${VAR} in keys, CLI command/credential, and argv. Placeholders use
	// {{double braces}}, so ExpandEnv leaves them untouched.
	for i := range c.Providers {
		p := &c.Providers[i]
		p.APIKey = os.ExpandEnv(p.APIKey)
		p.Command = os.ExpandEnv(p.Command)
		p.Credential = os.ExpandEnv(p.Credential)

		for j := range p.Args {
			p.Args[j] = os.ExpandEnv(p.Args[j])
		}
		for j := range p.HealthCommand {
			p.HealthCommand[j] = os.ExpandEnv(p.HealthCommand[j])
		}
	}
	return c, nil
}

// resolveModels back-fills Provider.Models from the models: routing table for
// providers that were declared without an inline models: list (the map format).
// A backend entry's model name is added to its provider's Models slice, in the
// order backends appear across all model definitions. Duplicates are skipped so
// the same model referenced in several virtual models isn't added twice.
func resolveModels(c *Config) {
	if len(c.Models) == 0 {
		return
	}
	// Build a name → index map for quick lookup.
	idx := make(map[string]int, len(c.Providers))
	for i, p := range c.Providers {
		idx[p.Name] = i
	}
	// seen[providerName][modelName] prevents duplicates.
	seen := make(map[string]map[string]bool)

	for _, m := range c.Models {
		for _, b := range m.Backends {
			if b.Model == "" {
				continue
			}
			i, ok := idx[b.Provider]
			if !ok {
				continue // backend references an unknown provider — skip silently
			}
			if seen[b.Provider] == nil {
				seen[b.Provider] = make(map[string]bool)
			}
			if seen[b.Provider][b.Model] {
				continue
			}
			seen[b.Provider][b.Model] = true
			c.Providers[i].Models = append(c.Providers[i].Models, b.Model)
		}
	}
}

// knownOutputs / knownKinds are the accepted enum values, checked by Validate so a
// typo (e.g. `kind: htpp`) is caught by `chicco -check` instead of silently
// behaving like the default.
var (
	knownOutputs    = map[string]bool{"": true, "text": true, "json": true}
	knownKinds      = map[string]bool{"": true, "http": true, "cli": true}
	knownStrategies = map[string]bool{"": true, "order": true, "round_robin": true, "random": true, "weighted": true}
)

// Validate checks a loaded Config for mistakes that would make a provider unusable
// or a field silently ignored, returning a human-readable problem for each. It is
// what `chicco -check` reports; an empty result means the config is sound. It does
// not open sockets or run any provider — a static check safe to run in CI or a
// pre-commit hook. Problems prefixed "warning:" don't make the config invalid (the
// provider is just skipped at startup); the rest are hard errors.
func (c Config) Validate() []string {
	var problems []string
	for i, m := range c.Models {
		where := m.ID
		if where == "" {
			where = fmt.Sprintf("models[%d]", i)
		}
		if !knownStrategies[m.Strategy] {
			problems = append(problems, "model "+where+": unknown strategy "+strconv.Quote(m.Strategy)+
				` (want "order", "round_robin", "random" or "weighted")`)
		}
	}
	if len(c.Providers) == 0 {
		return []string{"no providers configured"}
	}
	seen := map[string]bool{}
	active := 0
	for i, p := range c.Providers {
		where := p.Name
		if where == "" {
			where = fmt.Sprintf("providers[%d]", i)
			problems = append(problems, where+": missing name")
		}
		if p.Name != "" && seen[p.Name] {
			problems = append(problems, where+": duplicate provider name")
		}
		seen[p.Name] = true

		if !knownKinds[p.Kind] {
			problems = append(problems, where+": unknown kind "+strconv.Quote(p.Kind)+` (want "http" or "cli")`)
		}
		if !knownOutputs[p.Output] {
			problems = append(problems, where+": unknown output "+strconv.Quote(p.Output)+` (want "text" or "json")`)
		}
		if p.Output == "json" && p.ResultPath == "" {
			problems = append(problems, where+`: output: json needs a result_path`)
		}
		if p.Quota.RPM < 0 || p.Quota.RPH < 0 || p.Quota.RPD < 0 || p.Quota.TPM < 0 || p.Quota.TPH < 0 || p.Quota.TPD < 0 {
			problems = append(problems, where+": quota must not be negative")
		}
		if p.TimeoutSecs < 0 {
			problems = append(problems, where+": timeout_seconds must not be negative")
		}
		if p.Weight < 0 {
			problems = append(problems, where+": weight must not be negative")
		}

		if p.Kind == "cli" {
			if p.Command == "" {
				problems = append(problems, where+": kind: cli needs a command")
			}
		} else if p.BaseURL == "" {
			problems = append(problems, where+": missing base_url")
		}

		// Would this provider actually route? (mirrors Rotator.Active)
		switch {
		case len(p.Models) == 0:
			problems = append(problems, "warning: "+where+": no models — provider is inactive")
		case p.Kind != "cli" && p.APIKey == "":
			problems = append(problems, "warning: "+where+": no api_key (unset env var?) — provider is inactive")
		default:
			active++
		}
	}
	if active == 0 {
		problems = append(problems, "warning: no active providers (none have both models and, for http, an api_key)")
	}
	return problems
}
