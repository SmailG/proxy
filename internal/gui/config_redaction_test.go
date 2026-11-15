package gui

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/routatic/proxy/internal/config"
)

// A key distinctive enough that finding it anywhere in a response is a leak.
const leakTestKey = "sk-test-do-not-leak-0123456789"

// GET serves the in-memory config, where every ${VAR} is already resolved, so
// without redaction it hands the literal upstream keys to any local process and
// leaves them in the dashboard's own DOM.
func TestHandleProxyConfig_GETRedactsSecrets(t *testing.T) {
	cfg := &config.Config{
		Host:        "127.0.0.1",
		Port:        3456,
		APIKey:      leakTestKey,
		APIKeys:     []string{leakTestKey, leakTestKey},
		OpenCodeGo:  config.OpenCodeGoConfig{APIKey: leakTestKey},
		ModelStudio: config.ModelStudioConfig{APIKey: leakTestKey},
		JevRouting: config.JevRoutingConfig{
			Enabled:   true,
			Mode:      config.JevModeActive,
			APIKeyEnv: "TYPESAFE_AI_API_KEY",
		},
	}
	s := jevTestServer(filepath.Join(t.TempDir(), "config.json"), cfg)

	rec := httptest.NewRecorder()
	s.handleProxyConfig(rec, httptest.NewRequest(http.MethodGet, "/api/proxy/config", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, leakTestKey) {
		t.Errorf("GET leaked a secret:\n%s", body)
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if got["api_key"] != redactedSentinel {
		t.Errorf("api_key = %v, want the sentinel", got["api_key"])
	}
	// anonymizeMap collapses a pool to one sentinel rather than one per element,
	// so the response does not disclose how many keys are configured either.
	keys, _ := got["api_keys"].([]any)
	if len(keys) != 1 || keys[0] != redactedSentinel {
		t.Errorf("api_keys = %v, want a single sentinel", got["api_keys"])
	}
	for _, section := range []string{"opencode_go", "model_studio"} {
		provider, _ := got[section].(map[string]any)
		if provider["api_key"] != redactedSentinel {
			t.Errorf("%s.api_key = %v, want the sentinel", section, provider["api_key"])
		}
	}

	// api_key_env names an environment variable rather than holding a secret;
	// redacting it would hide which variable the classifier reads.
	jev, _ := got["jev_routing"].(map[string]any)
	if jev["api_key_env"] != "TYPESAFE_AI_API_KEY" {
		t.Errorf("jev_routing.api_key_env = %v, want the variable name preserved", jev["api_key_env"])
	}
}

// The Settings tab posts back what GET gave it, so a secret the user never
// touched arrives as the sentinel. Merging that would replace a working key —
// and a ${VAR} reference the loader would have resolved — with a placeholder.
func TestMergeConfigPatch_EchoedSentinelLeavesTheStoredKeyAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{
  "host": "127.0.0.1",
  "port": 3456,
  "api_key": "` + leakTestKey + `",
  "model_studio": {
    "api_key": "${BAILIAN_TOKEN_PLAN_API_KEY}",
    "api_keys": ["kept-1", "kept-2"]
  }
}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	patch := map[string]any{
		"host":    "127.0.0.1",
		"api_key": redactedSentinel,
		"model_studio": map[string]any{
			"api_key":  redactedSentinel,
			"api_keys": []any{redactedSentinel},
		},
	}
	out, err := mergeConfigPatch(path, patch)
	if err != nil {
		t.Fatalf("mergeConfigPatch: %v", err)
	}

	if bytes.Contains(out, []byte(redactedSentinel)) {
		t.Errorf("the sentinel was written into the config:\n%s", out)
	}
	if !bytes.Contains(out, []byte(leakTestKey)) {
		t.Errorf("the stored key was lost:\n%s", out)
	}
	if !bytes.Contains(out, []byte("${BAILIAN_TOKEN_PLAN_API_KEY}")) {
		t.Errorf("the ${VAR} reference was resolved away:\n%s", out)
	}
	if !bytes.Contains(out, []byte("kept-1")) || !bytes.Contains(out, []byte("kept-2")) {
		t.Errorf("the stored api_keys array was dropped:\n%s", out)
	}
}

// Redaction must not make a key uneditable: a value the user actually typed
// still replaces what is on disk.
func TestMergeConfigPatch_TypedSecretStillReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"host":"127.0.0.1","port":3456,"api_key":"` + leakTestKey + `"}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := mergeConfigPatch(path, map[string]any{"api_key": "sk-typed-by-the-user"})
	if err != nil {
		t.Fatalf("mergeConfigPatch: %v", err)
	}
	if !bytes.Contains(out, []byte("sk-typed-by-the-user")) {
		t.Errorf("the typed key was not written:\n%s", out)
	}
	if bytes.Contains(out, []byte(leakTestKey)) {
		t.Errorf("the old key survived a replacement:\n%s", out)
	}
}

// A list only counts as redacted when every element is the placeholder, so a
// real key pool cannot be discarded by one sentinel riding along in it.
func TestAllRedacted(t *testing.T) {
	tests := []struct {
		name string
		in   []any
		want bool
	}{
		{"empty is not redacted", []any{}, false},
		{"all placeholders", []any{redactedSentinel, redactedSentinel}, true},
		{"one real value", []any{redactedSentinel, "sk-real"}, false},
		{"non-string element", []any{redactedSentinel, 7}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := allRedacted(tc.in); got != tc.want {
				t.Errorf("allRedacted(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// Fallbacks is the only array-of-objects in the config, so it is the only place a
// nested secret could hide from a map-only walk. extra_body survives the typed
// round-trip as raw JSON, which is what makes this reachable at all — ModelConfig
// itself has no api_key field, so one written at the entry level never reaches the
// response.
func TestHandleProxyConfig_GETRedactsSecretsInsideFallbackEntries(t *testing.T) {
	cfg := &config.Config{
		Host: "127.0.0.1",
		Port: 3456,
		Fallbacks: map[string][]config.ModelConfig{
			"default": {{
				Provider: "model-studio",
				ModelID:  "qwen3.7-max",
				ExtraBody: map[string]json.RawMessage{
					"api_key": json.RawMessage(`"` + leakTestKey + `"`),
				},
			}},
		},
	}
	s := jevTestServer(filepath.Join(t.TempDir(), "config.json"), cfg)

	rec := httptest.NewRecorder()
	s.handleProxyConfig(rec, httptest.NewRequest(http.MethodGet, "/api/proxy/config", nil))

	if strings.Contains(rec.Body.String(), leakTestKey) {
		t.Errorf("GET leaked a secret nested in a fallback entry:\n%s", rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	fallbacks, _ := got["fallbacks"].(map[string]any)
	chain, _ := fallbacks["default"].([]any)
	if len(chain) != 1 {
		t.Fatalf("fallbacks.default = %v, want one entry", fallbacks["default"])
	}
	entry, _ := chain[0].(map[string]any)
	extra, _ := entry["extra_body"].(map[string]any)
	if extra["api_key"] != redactedSentinel {
		t.Errorf("fallbacks.default[0].extra_body.api_key = %v, want the sentinel", extra["api_key"])
	}
	// The rest of the entry is not a secret and has to survive, or the Fallback tab
	// would render a chain it cannot edit.
	if entry["model_id"] != "qwen3.7-max" {
		t.Errorf("model_id = %v, want it preserved", entry["model_id"])
	}
}

// extra_body is arbitrary vendor JSON preserved verbatim by the typed
// round-trip, so a secret can sit one level deeper than a single pass reaches:
// inside an object under a sensitive key, and inside an array inside an array.
// Both shapes were served in clear.
func TestAnonymizeConfig_RedactsSecretsNestedInExtraBody(t *testing.T) {
	cfg := &config.Config{
		Host: "127.0.0.1",
		Port: 3456,
		Models: map[string]config.ModelConfig{
			"default": {
				Provider: "model-studio",
				ModelID:  "qwen3.7-max",
				ExtraBody: map[string]json.RawMessage{
					"credentials": json.RawMessage(`{"api_key":"` + leakTestKey + `"}`),
					"payload":     json.RawMessage(`[[{"api_key":"` + leakTestKey + `"}]]`),
				},
			},
		},
	}

	out := anonymizeConfig(cfg)
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(leakTestKey)) {
		t.Errorf("a secret nested in extra_body was served in clear:\n%s", body)
	}
	// The shape has to survive as well, or the field cannot be displayed at all.
	extra := out.Models["default"].ExtraBody
	if !bytes.Contains(extra["credentials"], []byte(redactedSentinel)) {
		t.Errorf("credentials = %s, want the nested api_key redacted", extra["credentials"])
	}
	if !bytes.Contains(extra["payload"], []byte(redactedSentinel)) {
		t.Errorf("payload = %s, want the api_key inside the nested array redacted", extra["payload"])
	}
}

// A placeholder beside real keys cannot be dropped without discarding the stored
// pool, and cannot be merged without saving the placeholder as a credential. It is
// refused rather than guessed at.
func TestMergeConfigPatch_MixedPlaceholderListIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"host":"127.0.0.1","port":3456,"api_keys":["kept-1","kept-2"]}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := mergeConfigPatch(path, map[string]any{
		"api_keys": []any{redactedSentinel, "sk-new"},
	})
	if !errors.Is(err, errRedactedEcho) {
		t.Fatalf("err = %v, want errRedactedEcho", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(after, []byte("kept-1")) || bytes.Contains(after, []byte(redactedSentinel)) {
		t.Errorf("the stored pool changed on a refused patch:\n%s", after)
	}
}

// mergeJSONMap replaces arrays wholesale, so a placeholder nested inside one
// cannot be dropped without losing the stored value. fallbacks is reconciled
// against the stored document instead: the entry is matched by provider and
// model_id, and the placeholder is replaced by what GET hid. This is the exact
// shape the Fallback tab echoes back on every save.
func TestMergeConfigPatch_FallbackPlaceholderIsRestoredFromTheStoredEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"host":"127.0.0.1","port":3456,"fallbacks":{"default":[{"provider":"model-studio","model_id":"qwen3.7-max","extra_body":{"api_key":"` + leakTestKey + `"}}]}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := mergeConfigPatch(path, map[string]any{
		"fallbacks": map[string]any{
			"default": []any{map[string]any{
				"provider":   "model-studio",
				"model_id":   "qwen3.7-max",
				"extra_body": map[string]any{"api_key": redactedSentinel},
			}},
		},
	})
	if err != nil {
		t.Fatalf("mergeConfigPatch: %v", err)
	}
	if !bytes.Contains(out, []byte(leakTestKey)) {
		t.Errorf("the stored key was not restored:\n%s", out)
	}
	if bytes.Contains(out, []byte(redactedSentinel)) {
		t.Errorf("the placeholder was saved as a credential:\n%s", out)
	}
}

// A placeholder on an entry the stored chain does not hold has nothing to be
// restored from. Matching by position instead could graft one model's key onto
// another, so it is refused.
func TestMergeConfigPatch_FallbackPlaceholderWithNoStoredEntryIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"host":"127.0.0.1","port":3456,"fallbacks":{"default":[{"provider":"model-studio","model_id":"qwen3.7-max","extra_body":{"api_key":"` + leakTestKey + `"}}]}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := mergeConfigPatch(path, map[string]any{
		"fallbacks": map[string]any{
			"default": []any{map[string]any{
				"provider":   "model-studio",
				"model_id":   "a-model-that-is-not-stored",
				"extra_body": map[string]any{"api_key": redactedSentinel},
			}},
		},
	})
	if !errors.Is(err, errRedactedEcho) {
		t.Fatalf("err = %v, want errRedactedEcho", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(after, []byte(leakTestKey)) {
		t.Errorf("the stored key changed on a refused patch:\n%s", after)
	}
}

// The Fallback tab posts back every chain it was served, including scenarios the
// user never touched, so one redacted secret anywhere used to fail the whole
// save with a 400 — including an edit to an unrelated scenario, which is most
// saves. This drives the real handlers: GET, then POST the chains verbatim with
// one edit to a chain that holds no secret.
func TestHandleProxyConfig_FallbackTabRoundTripSaves(t *testing.T) {
	t.Setenv("ROUTATIC_PROXY_API_KEY", "test-key")
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"host":"127.0.0.1","port":3456,"fallbacks":{"default":[{"provider":"model-studio","model_id":"qwen3.7-max","extra_body":{"api_key":"` + leakTestKey + `"}}],"complex":[{"provider":"opencode-go","model_id":"glm-5.2"}]}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	s := jevTestServer(path, &config.Config{
		Host: "127.0.0.1",
		Port: 3456,
		Fallbacks: map[string][]config.ModelConfig{
			"default": {{
				Provider: "model-studio",
				ModelID:  "qwen3.7-max",
				ExtraBody: map[string]json.RawMessage{
					"api_key": json.RawMessage(`"` + leakTestKey + `"`),
				},
			}},
			"complex": {{Provider: "opencode-go", ModelID: "glm-5.2"}},
		},
	})

	get := httptest.NewRecorder()
	s.handleProxyConfig(get, httptest.NewRequest(http.MethodGet, "/api/proxy/config", nil))
	var served map[string]any
	if err := json.Unmarshal(get.Body.Bytes(), &served); err != nil {
		t.Fatalf("decoding the GET response: %v", err)
	}
	chains, _ := served["fallbacks"].(map[string]any)
	if len(chains) != 2 {
		t.Fatalf("GET served %d chains, want 2: %v", len(chains), chains)
	}

	// parseFallbackChain shallow-copies each entry and save() spreads every chain
	// into the patch; the only edit here is to the chain with no secret.
	edited, _ := chains["complex"].([]any)
	edited = append(edited, map[string]any{"provider": "opencode-go", "model_id": "kimi-k2.6"})
	chains["complex"] = edited

	post := postProxyConfig(s, map[string]any{"fallbacks": chains})
	if post.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", post.Code, post.Body.String())
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(after, []byte(leakTestKey)) {
		t.Errorf("the chain nobody edited lost its key:\n%s", after)
	}
	if bytes.Contains(after, []byte(redactedSentinel)) {
		t.Errorf("the placeholder was written to disk:\n%s", after)
	}
	if !bytes.Contains(after, []byte("kimi-k2.6")) {
		t.Errorf("the edit to the other chain was not saved:\n%s", after)
	}
}

// The refusal is the client's doing, so it must not be reported as a server fault:
// a 500 tells the user nothing about which field to retype.
func TestHandleProxyConfig_EchoedPlaceholderIsABadRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"host":"127.0.0.1","port":3456,"api_keys":["kept-1"]}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	s := jevTestServer(path, &config.Config{Host: "127.0.0.1", Port: 3456, APIKeys: []string{"kept-1"}})

	rec := postProxyConfig(s, map[string]any{"api_keys": []any{redactedSentinel, "sk-new"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "hidden") {
		t.Errorf("body = %q, want it to say the value is hidden", rec.Body.String())
	}
}
