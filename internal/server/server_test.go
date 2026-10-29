package server

import (
	"slices"
	"testing"

	"github.com/routatic/proxy/internal/config"
	"github.com/routatic/proxy/internal/core"
	"github.com/routatic/proxy/internal/provider"
)

// A provider that is absent from the registry is not rejected — the handlers
// fall through to the legacy OpenCode client, which knows nothing about its
// endpoint. That failure looks like a working proxy routing to the wrong host,
// so the registry set is asserted here rather than discovered at runtime.
func TestNewProviderRegistry_RegistersModelStudio(t *testing.T) {
	registry, err := newProviderRegistry(config.NewAtomicConfig(&config.Config{}, ""))
	if err != nil {
		t.Fatal(err)
	}

	p, ok := registry.Get(config.ProviderModelStudio)
	if !ok {
		t.Fatalf("model-studio is not registered; registry holds %v", registry.List())
	}
	if p.Name() != config.ProviderModelStudio {
		t.Errorf("registered provider Name() = %q, want %q", p.Name(), config.ProviderModelStudio)
	}
}

// Every provider that has a core.Provider implementation must be registered.
// OpenRouter is excluded on purpose: it is served by the legacy client.
func TestNewProviderRegistry_CoversEveryImplementedProvider(t *testing.T) {
	registry, err := newProviderRegistry(config.NewAtomicConfig(&config.Config{}, ""))
	if err != nil {
		t.Fatal(err)
	}
	registered := registry.List()

	want := []string{
		config.ProviderOpenCodeGo,
		config.ProviderOpenCodeZen,
		config.ProviderAWSBedrock,
		config.ProviderModelStudio,
	}
	for _, name := range want {
		if !slices.Contains(registered, name) {
			t.Errorf("provider %q is implemented but not registered; registry holds %v", name, registered)
		}
	}

	if _, ok := registry.Get(config.ProviderOpenRouter); ok {
		t.Error("openrouter is registered; it is intentionally served by the legacy client")
	}
}

// The registry is what the handlers consult to choose the provider path, so
// each registered provider must resolve to a distinct implementation.
func TestNewProviderRegistry_WireFormatPerProvider(t *testing.T) {
	registry, err := newProviderRegistry(config.NewAtomicConfig(&config.Config{
		ModelStudio: config.ModelStudioConfig{BaseURL: "https://example.invalid/v1"},
	}, ""))
	if err != nil {
		t.Fatal(err)
	}

	p, ok := registry.Get(config.ProviderModelStudio)
	if !ok {
		t.Fatal("model-studio is not registered")
	}
	// No anthropic_base_url configured, so Model Studio must use Chat Completions.
	if got := p.WireFormat(config.ModelConfig{ModelID: "qwen3.8-max"}); got != core.WireFormatOpenAIChat {
		t.Errorf("model-studio WireFormat() = %v, want WireFormatOpenAIChat when only base_url is set", got)
	}
}

// Guards against a new provider type being added without a registration line.
func TestProviderConstructorsAreDistinct(t *testing.T) {
	atomic := config.NewAtomicConfig(&config.Config{}, "")

	names := []string{
		provider.NewOpenCodeGoProvider(atomic).Name(),
		provider.NewOpenCodeZenProvider(atomic).Name(),
		provider.NewAWSBedrockProvider(atomic).Name(),
		provider.NewModelStudioProvider(atomic).Name(),
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("duplicate provider name %q in %v", n, names)
		}
		seen[n] = true
	}
}
