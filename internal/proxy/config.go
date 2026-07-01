package proxy

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// DefaultAddr is the port chicco listens on when chicco.yaml sets no addr.
const DefaultAddr = ":41986"

// Config is chicco.yaml.
type Config struct {
	Addr      string     `yaml:"addr"`
	Providers []Provider // populated by UnmarshalYAML from either a list or a map
	Models    []Model    `yaml:"models"` // forward-looking routing table (not yet wired up)
}

// rawConfig is the intermediate shape used during YAML decoding. providers is
// kept as a raw yaml.Node so we can detect list vs. map and preserve order.
type rawConfig struct {
	Addr      string    `yaml:"addr"`
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

// Provider is one upstream OpenAI-compatible endpoint. APIKey is sent as a Bearer
// token (it is run through os.ExpandEnv on load, so `${GROQ_API_KEY}` works);
// Models is round-robined per provider. A provider with an empty key or no models
// is inactive and skipped.
type Provider struct {
	Name    string   `yaml:"name"`
	BaseURL string   `yaml:"base_url"`
	APIKey  string   `yaml:"api_key"`
	Models  []string `yaml:"models"`
	// QuotaTokens / QuotaRequests are the provider's budget for its quota window,
	// used only to drive the dashboard's usage bar. Tokens take priority; a
	// request budget covers providers capped by request count (e.g. Google's free
	// tier) rather than tokens. 0/0 shows usage without a bar.
	QuotaTokens   int64  `yaml:"quota_tokens"`
	QuotaRequests int    `yaml:"quota_requests"`
	Window        string `yaml:"window"` // "daily" | "monthly" | "none" (default): when usage counters reset

	// CLI provider fields (kind: cli). Instead of an HTTP call, chicco runs Command
	// with Args (templated with {{model}}/{{system}}/{{user}}/{{prompt}}/{{output_file}}),
	// buffers the completion, and synthesizes the OpenAI SSE the caller expects.
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

// Model is a named virtual model that routes across one or more provider backends.
// Not yet wired into the rotator — present so the models: section parses cleanly.
type Model struct {
	ID       string    `yaml:"id"`
	Strategy string    `yaml:"strategy"` // "rotate" | "failover"
	Backends []Backend `yaml:"backends"`
}

// Backend is one entry in a Model's backend list.
type Backend struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
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
