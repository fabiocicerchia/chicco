package proxy

import (
	"fmt"
	"sort"
	"strconv"
)

// validate.go is `chicco -check`: the static reading of a loaded config that
// names every field that would be silently ignored or leave a provider unused.

// knownOutputs / knownKinds are the accepted enum values, checked by Validate so a
// typo (e.g. `kind: htpp`) is caught by `chicco -check` instead of silently
// behaving like the default.
var (
	knownOutputs    = map[string]bool{"": true, "text": true, "json": true}
	knownKinds      = map[string]bool{"": true, "http": true, "cli": true}
	knownStrategies = map[string]bool{"": true, "order": true, "round_robin": true, "random": true, "weighted": true}
)

// Validate - Checks a loaded Config for mistakes that would make a provider
// unusable or a field silently ignored, returning a human-readable problem for
// each. It is what `chicco -check` reports; an empty result means the config is
// sound. It does not open sockets or run any provider — a static check safe to
// run in CI or a pre-commit hook. Problems prefixed "warning:" don't make the
// config invalid (the provider is just skipped at startup); the rest are hard
// errors.
func (c Config) Validate() []string {
	var problems []string
	if quotaNegative(c.Quota) {
		problems = append(problems, "quota: must not be negative")
	}
	problems = append(problems, validateModels(c.Models)...)
	if len(c.Providers) == 0 {
		return []string{"no providers configured"}
	}
	return append(problems, validateProviders(c.Providers)...)
}

// quotaNegative - Reports whether any limit in q is negative. One copy, because
// a window added to Quota and missed in one of the two callers would be a limit
// nothing checks.
func quotaNegative(q Quota) bool {
	return q.RPM < 0 || q.RPH < 0 || q.RPD < 0 || q.TPM < 0 || q.TPH < 0 || q.TPD < 0
}

// validateModels - Checks the virtual model routing table: a strategy chicco
// knows, and non-negative backend weights.
func validateModels(models []Model) []string {
	var problems []string
	for i, m := range models {
		where := m.ID
		if where == "" {
			where = fmt.Sprintf("models[%d]", i)
		}
		if !knownStrategies[m.Strategy] {
			problems = append(problems, "model "+where+": unknown strategy "+strconv.Quote(m.Strategy)+
				` (want "order", "round_robin", "random" or "weighted")`)
		}
		for _, b := range m.Backends {
			if b.Weight != nil && *b.Weight < 0 {
				problems = append(problems, "model "+where+": backend "+b.Provider+": weight must not be negative")
			}
		}
	}
	return problems
}

// validateProviders - Checks the provider list: identity (a name, and a unique
// one), each provider's own fields, and whether anything would route at all.
func validateProviders(providers []Provider) []string {
	var problems []string
	seen := map[string]bool{}
	active := 0
	for i, p := range providers {
		where := p.Name
		if where == "" {
			where = fmt.Sprintf("providers[%d]", i)
			problems = append(problems, where+": missing name")
		}
		if p.Name != "" && seen[p.Name] {
			problems = append(problems, where+": duplicate provider name")
		}
		seen[p.Name] = true

		problems = append(problems, validateProvider(p, where)...)

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

// validateProvider - Checks one provider's own fields, in the order they are
// reported: enumerations first, then the numbers, then the field the chosen
// kind makes mandatory. where is the label already resolved for this entry.
func validateProvider(p Provider, where string) []string {
	var problems []string
	if !knownKinds[p.Kind] {
		problems = append(problems, where+": unknown kind "+strconv.Quote(p.Kind)+` (want "http" or "cli")`)
	}
	if !knownOutputs[p.Output] {
		problems = append(problems, where+": unknown output "+strconv.Quote(p.Output)+` (want "text" or "json")`)
	}
	if p.Output == "json" && p.ResultPath == "" {
		problems = append(problems, where+`: output: json needs a result_path`)
	}
	if quotaNegative(p.Quota) {
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
	return problems
}

// validateAliases refuses an alias pointing at nothing.
//
// Caught at load rather than at request time on purpose: an alias that
// silently falls through to full rotation is the failure the issue exists to
// remove — the caller asked for a specific routing policy and would get an
// arbitrary one, with nothing in the logs saying so. A typo in chicco.yaml
// should stop the proxy starting, the same way a bad addr does.
func validateAliases(c *Config) error {
	if len(c.Aliases) == 0 {
		return nil
	}
	known := make(map[string]bool, len(c.Models)+1)
	known["chicco:auto"] = true
	for _, m := range c.Models {
		known[m.ID] = true
	}
	names := make([]string, 0, len(c.Aliases))
	for name := range c.Aliases {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic message when several are wrong
	for _, name := range names {
		target := c.Aliases[name]
		switch {
		case target == "":
			return fmt.Errorf("alias %q has no target", name)
		case known[name]:
			// Shadowing a real model id would make routing depend on lookup
			// order, which is not something to leave to chance.
			return fmt.Errorf("alias %q has the same name as a model", name)
		case !known[target]:
			return fmt.Errorf("alias %q points at %q, which is not a model in this config", name, target)
		}
	}
	return nil
}
