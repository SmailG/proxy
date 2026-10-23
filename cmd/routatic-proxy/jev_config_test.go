package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The shipped config must carry the jev_routing block switched off, so a fresh
// install sends nothing to TypeSafe until the user opts in.
func TestDefaultConfigJevRoutingIsOffAndMatchesExample(t *testing.T) {
	def := mustParseConfig(t, getDefaultConfig())
	defJev, ok := def["jev_routing"].(map[string]any)
	if !ok {
		t.Fatal("jev_routing missing from the default config template")
	}
	if enabled, _ := defJev["enabled"].(bool); enabled {
		t.Error("jev_routing.enabled must be false in the default config template")
	}
	if defJev["mode"] != "active" {
		t.Errorf("default mode = %v, want active", defJev["mode"])
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "configs", "config.example.json"))
	if err != nil {
		t.Fatalf("read config.example.json: %v", err)
	}
	var example map[string]any
	if err := json.Unmarshal(raw, &example); err != nil {
		t.Fatalf("parse config.example.json: %v", err)
	}
	if !reflect.DeepEqual(defJev, example["jev_routing"]) {
		t.Errorf("jev_routing differs between default template and config.example.json:\n  default: %v\n  example: %v", defJev, example["jev_routing"])
	}
}
