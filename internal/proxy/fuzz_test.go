package proxy

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// FuzzConfigParsing drives arbitrary bytes through the same YAML decode +
// validation path chicco uses to load chicco.yaml. The custom UnmarshalYAML
// juggles providers-as-list vs. providers-as-map and unwraps document nodes,
// and Validate walks the whole decoded tree — both are the kind of hand-rolled
// parsing that hostile input can panic. The property under test: malformed
// input is rejected with an error, and anything that decodes survives
// validation, but neither ever crashes the process.
func FuzzConfigParsing(f *testing.F) {
	seeds := []string{
		"",
		"addr: \":8080\"\n",
		"providers:\n  - name: a\n    base_url: http://x\n    api_key: k\n    models: [m]\n",
		"providers:\n  a:\n    base_url: http://x\n    api_key: k\n    models: [m]\n",
		"quota:\n  rpm: 100\n  tpd: 1000\n",
		"models:\n  - id: gpt\n    backends:\n      - provider: a\n        model: m\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var cfg Config
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return // rejected cleanly; nothing more to exercise
		}
		// A config that decoded must run through validation without panicking.
		_ = cfg.Validate()
	})
}
