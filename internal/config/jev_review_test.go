package config

import "testing"

// Review finding 8: the key and the prompt travel to base_url, so plain http is only
// acceptable for loopback.
func TestValidateJevRouting_BaseURLSchemeAndHost(t *testing.T) {
	ok := func(u string) JevRoutingConfig {
		return JevRoutingConfig{Enabled: true, Mode: JevModeShadow, Model: "m", TimeoutMs: 300, APIKeyEnv: "TYPESAFE_AI_API_KEY", BaseURL: u}
	}
	accept := []string{
		"https://api.typesafe.ai", "https://example.com:8443/x",
		"http://127.0.0.1:9999", "http://localhost:8080", "http://LOCALHOST", "http://[::1]:7000", "http://127.1.2.3",
	}
	reject := []string{
		"http://example.com", "http://api.typesafe.ai", "http://10.0.0.5:80", "http://127.0.0.1.evil.com",
		"https://", "http://", "ftp://api.typesafe.ai", "api.typesafe.ai", "https:///path-only", "https://:443", "http://:8080",
	}
	for _, u := range accept {
		if err := validateJevRouting(ok(u)); err != nil {
			t.Errorf("base_url %q rejected: %v", u, err)
		}
	}
	for _, u := range reject {
		if err := validateJevRouting(ok(u)); err == nil {
			t.Errorf("base_url %q accepted", u)
		}
	}
}

func TestValidateJevRouting_TimeoutBoundaries(t *testing.T) {
	with := func(ms int) error {
		return validateJevRouting(JevRoutingConfig{Enabled: true, Mode: JevModeShadow, Model: "m", TimeoutMs: ms, APIKeyEnv: "X_KEY", BaseURL: "https://api.typesafe.ai"})
	}
	for _, ms := range []int{1, 300, maxJevTimeoutMs} {
		if err := with(ms); err != nil {
			t.Errorf("timeout_ms %d rejected: %v", ms, err)
		}
	}
	for _, ms := range []int{0, -1, maxJevTimeoutMs + 1} {
		if err := with(ms); err == nil {
			t.Errorf("timeout_ms %d accepted", ms)
		}
	}
	if maxJevTimeoutMs != 10000 {
		t.Errorf("maxJevTimeoutMs = %d; CONFIGURATION.md documents 10000", maxJevTimeoutMs)
	}
}

// CheckJevRouting is what the dashboard import calls before it writes the file.
func TestCheckJevRouting_AppliesDefaultsThenValidates(t *testing.T) {
	if err := CheckJevRouting(JevRoutingConfig{}); err != nil {
		t.Errorf("a disabled block is inert, got %v", err)
	}
	if err := CheckJevRouting(JevRoutingConfig{Enabled: true}); err != nil {
		t.Errorf("an enabled block with everything defaulted must pass, got %v", err)
	}
	if err := CheckJevRouting(JevRoutingConfig{Enabled: true, Mode: "turbo"}); err == nil {
		t.Error("a bad mode must be rejected")
	}
	if err := CheckJevRouting(JevRoutingConfig{Enabled: true, APIKeyEnv: "***REDACTED***"}); err == nil {
		t.Error("a redacted api_key_env must be rejected")
	}
	if err := CheckJevRouting(JevRoutingConfig{Enabled: true, BaseURL: "http://example.com"}); err == nil {
		t.Error("cleartext http to a remote host must be rejected")
	}
}
