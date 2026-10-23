package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/routatic/proxy/internal/config"
)

// Review finding 9: api_key_env is the NAME of an environment variable, not a secret.
func TestShouldAnonymize_KeepsApiKeyEnvRedactsSecrets(t *testing.T) {
	if shouldAnonymize("api_key_env") {
		t.Error("api_key_env must survive the anonymized export")
	}
	for _, k := range []string{"api_key", "api_keys", "token", "client_secret", "password", "credential"} {
		if !shouldAnonymize(k) {
			t.Errorf("%q must still be redacted", k)
		}
	}
}

// Review finding 9: an import must be validated before the working config on disk is replaced.
func TestHandleConfigImport_BadJevBlockDoesNotOverwriteTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := []byte(`{"host":"127.0.0.1","port":3456}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{atomicCfg: config.NewAtomicConfig(&config.Config{Host: "127.0.0.1", Port: 3456}, path)}

	bad := map[string]any{"config": map[string]any{
		"host": "127.0.0.1", "port": 3456,
		"jev_routing": map[string]any{"enabled": true, "api_key_env": "***REDACTED***"},
	}, "apply": true}
	body, _ := json.Marshal(bad)
	rec := httptest.NewRecorder()
	s.handleConfigImport(rec, httptest.NewRequest(http.MethodPost, "/api/config/import", bytes.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a jev_routing block the loader would reject", rec.Code)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, original) {
		t.Errorf("the config file was replaced before validation: %s", got)
	}
}
