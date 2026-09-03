package proxy

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// configload.go reads chicco.yaml off disk: parse, default, expand ${VAR} and
// back-fill the models a keyed provider entry left implicit.

// LoadConfig - Reads and parses chicco.yaml, defaulting the listen address and
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

	if err := validateAliases(&c); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", path, err)
	}

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

// resolveModels - Back-fills Provider.Models from the models: routing table for
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
