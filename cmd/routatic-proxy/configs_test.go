package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/routatic/proxy/internal/config"
)

// TestShippedExampleConfigsAreConsistent guards the reference configs under
// configs/ against the two mistakes that are easy to make by hand and invisible
// until a request fails at runtime: a provider name that the loader would
// reject, and a model that references a provider block the file never defines.
func TestShippedExampleConfigsAreConsistent(t *testing.T) {
	dir := filepath.Join("..", "..", "configs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	var checked int
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			var cfg map[string]any
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatalf("not valid JSON: %v", err)
			}

			for _, m := range modelEntries(t, cfg) {
				provider, _ := m["provider"].(string)
				if provider == "" {
					// An empty provider means "inherit the default", which the
					// loader normalizes to opencode-go.
					continue
				}
				if !config.IsKnownProvider(provider) {
					t.Errorf("provider %q is not one of %v", provider, config.KnownProviders)
					continue
				}
				if id, _ := m["model_id"].(string); id == "" {
					t.Errorf("model entry for provider %q has no model_id", provider)
				}

				block := strings.ReplaceAll(config.NormalizeProvider(provider), "-", "_")
				if _, ok := cfg[block]; !ok {
					t.Errorf("references provider %q but the config defines no %q block", provider, block)
				}
			}
			checked++
		})
	}

	if checked == 0 {
		t.Fatal("no configs/*.json files were checked")
	}
}

// modelEntries flattens every place a config can declare a model: the scenario
// map, the per-scenario fallback arrays, and both override maps.
func modelEntries(t *testing.T, cfg map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any

	appendEntry := func(v any, where string) {
		m, ok := v.(map[string]any)
		if !ok {
			t.Errorf("%s: expected an object, got %T", where, v)
			return
		}
		out = append(out, m)
	}

	for _, section := range []string{"models", "model_overrides", "model_family_overrides"} {
		raw, ok := cfg[section]
		if !ok {
			continue
		}
		entries, ok := raw.(map[string]any)
		if !ok {
			t.Errorf("%s: expected an object, got %T", section, raw)
			continue
		}
		for alias, entry := range entries {
			appendEntry(entry, section+"."+alias)
		}
	}

	if raw, ok := cfg["fallbacks"]; ok {
		byScenario, ok := raw.(map[string]any)
		if !ok {
			t.Errorf("fallbacks: expected an object, got %T", raw)
		} else {
			for scenario, list := range byScenario {
				arr, ok := list.([]any)
				if !ok {
					t.Errorf("fallbacks.%s: expected an array, got %T", scenario, list)
					continue
				}
				for i, entry := range arr {
					appendEntry(entry, "fallbacks."+scenario+"["+strconv.Itoa(i)+"]")
				}
			}
		}
	}

	return out
}
