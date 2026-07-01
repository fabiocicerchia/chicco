package proxy

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// DefaultAddr is the port chicco listens on when chicco.yaml sets no addr.
const DefaultAddr = ":41986"

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

// Config is chicco.yaml.
type Config struct {
	Addr      string     `yaml:"addr"`
	Providers []Provider `yaml:"providers"`
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
