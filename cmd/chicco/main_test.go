package main

import (
	"os"
	"path/filepath"
	"testing"
)

// oneInactiveProvider - Writes a config whose only HTTP provider expands to an
// empty api_key, which is the shape that makes chicco warn and drop it. The CLI
// provider is there so the config still has something to serve with, i.e. so
// the failure under -strict is the warning and nothing else.
func oneInactiveProvider(t *testing.T) string {
	t.Helper()
	t.Setenv("CHICCO_TEST_MISSING_KEY", "")
	path := filepath.Join(t.TempDir(), "chicco.yaml")
	config := `providers:
  - name: anthropic-primary
    kind: http
    base_url: https://api.anthropic.com
    api_key: ${CHICCO_TEST_MISSING_KEY}
    models: [claude-sonnet-5]
  - name: local-cli
    kind: cli
    command: "echo"
    models: [local]
`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCheckAcceptsAnInactiveProvider - An unset key is normal on a laptop and
// in CI, so the default -check still passes: it warns and exits 0.
func TestCheckAcceptsAnInactiveProvider(t *testing.T) {
	if got := checkConfig(oneInactiveProvider(t), false); got != 0 {
		t.Fatalf("checkConfig(strict=false) = %d, want 0", got)
	}
}

// TestStrictCheckFailsOnAnInactiveProvider - -strict is for the deployment that
// means every declared provider: starting with half of them is a config error
// there, and nothing downstream notices on its own.
func TestStrictCheckFailsOnAnInactiveProvider(t *testing.T) {
	if got := checkConfig(oneInactiveProvider(t), true); got != 1 {
		t.Fatalf("checkConfig(strict=true) = %d, want 1", got)
	}
}

// TestStrictCheckPassesAValidConfig - -strict fails on warnings, not on their
// absence: a config with nothing to say about still exits 0.
func TestStrictCheckPassesAValidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chicco.yaml")
	config := `providers:
  - name: local-cli
    kind: cli
    command: "echo"
    models: [local]
`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := checkConfig(path, true); got != 0 {
		t.Fatalf("checkConfig(strict=true) on a clean config = %d, want 0", got)
	}
}
