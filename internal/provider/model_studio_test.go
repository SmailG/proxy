package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/routatic/proxy/internal/config"
	"github.com/routatic/proxy/internal/core"
	"github.com/routatic/proxy/pkg/types"
)

func TestModelStudioProvider_Name(t *testing.T) {
	p := NewModelStudioProvider(nil)
	if got := p.Name(); got != "model-studio" {
		t.Errorf("Name() = %q, want %q", got, "model-studio")
	}
}

// Native Anthropic passthrough is preferred whenever anthropic_base_url is set,
// because it avoids the lossy Anthropic -> OpenAI -> Anthropic round trip.
// Model Studio has no Responses or Gemini endpoint, so those overrides must
// fall through to the default rather than being honored.
func TestModelStudioProvider_WireFormat(t *testing.T) {
	const anthropicURL = "https://token-plan.ap-southeast-1.maas.aliyuncs.com/apps/anthropic"

	tests := []struct {
		name          string
		anthropicBase string
		wireFormat    string
		toolsDisabled bool
		want          core.WireFormat
	}{
		{"chat only, no override", "", "", false, core.WireFormatOpenAIChat},
		{"chat only, explicit openai", "", "openai", false, core.WireFormatOpenAIChat},
		{"chat only, anthropic requested but unavailable", "", "anthropic", false, core.WireFormatOpenAIChat},
		{"anthropic configured, no override", anthropicURL, "", false, core.WireFormatAnthropic},
		{"anthropic configured, explicit anthropic", anthropicURL, "anthropic", false, core.WireFormatAnthropic},
		{"anthropic configured, explicit openai opts out", anthropicURL, "openai", false, core.WireFormatOpenAIChat},
		{"anthropic configured, tools disabled forces chat", anthropicURL, "", true, core.WireFormatOpenAIChat},
		// Documented precedence: an explicit wire_format outranks
		// anthropic_tools_disabled, so the two together resolve to the override.
		{"explicit openai outranks tools disabled", anthropicURL, "openai", true, core.WireFormatOpenAIChat},
		{"explicit anthropic outranks tools disabled", anthropicURL, "anthropic", true, core.WireFormatAnthropic},
		{"anthropic configured, responses override ignored", anthropicURL, "responses", false, core.WireFormatAnthropic},
		{"anthropic configured, gemini override ignored", anthropicURL, "gemini", false, core.WireFormatAnthropic},
		{"anthropic configured, auto falls through", anthropicURL, "auto", false, core.WireFormatAnthropic},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{ModelStudio: config.ModelStudioConfig{
				BaseURL:          "https://dashscope-intl.aliyuncs.com/compatible-mode/v1",
				AnthropicBaseURL: tt.anthropicBase,
			}}
			p := NewModelStudioProvider(config.NewAtomicConfig(cfg, ""))

			model := config.ModelConfig{
				ModelID:                "qwen3.8-max",
				WireFormat:             tt.wireFormat,
				AnthropicToolsDisabled: tt.toolsDisabled,
			}
			if got := p.WireFormat(model); got != tt.want {
				t.Errorf("WireFormat() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Model Studio hosts many families that are not mirrored in the local registry;
// rejecting unknown IDs would make every new Token Plan model unusable.
func TestModelStudioProvider_ModelCapabilities_AcceptsUnknownModels(t *testing.T) {
	p := NewModelStudioProvider(nil)
	caps, ok := p.ModelCapabilities("qwen3.8-max")
	if !ok {
		t.Fatal("ModelCapabilities() returned false, want true")
	}
	// SupportsThinking and SupportsImageInput are the two flags
	// core.ValidateRequestCompatibility uses to drop a model from the chain, so
	// a false here would silently reject thinking and vision requests.
	if !caps.SupportsStreaming {
		t.Error("SupportsStreaming = false, want true")
	}
	if !caps.SupportsTools {
		t.Error("SupportsTools = false, want true")
	}
	if !caps.SupportsThinking {
		t.Error("SupportsThinking = false, want true")
	}
	if !caps.SupportsImageInput {
		t.Error("SupportsImageInput = false, want true")
	}
}

// An unknown model must also survive the compatibility check that gates the
// chain, not just ModelCapabilities.
func TestModelStudioProvider_ValidateRequest_AcceptsThinkingAndImageBlocks(t *testing.T) {
	p := NewModelStudioProvider(config.NewAtomicConfig(&config.Config{}, ""))

	req := &core.NormalizedRequest{
		Model: "qwen3.8-max",
		Messages: []core.NormalizedMessage{
			{Role: "user", Blocks: []core.NormalizedContentBlock{
				{Type: "text", Text: "describe this"},
				{Type: "image", Image: &core.NormalizedImage{MediaType: "image/png", Data: "AAAA"}},
			}},
			{Role: "assistant", Blocks: []core.NormalizedContentBlock{
				{Type: "thinking", Thinking: "hmm"},
			}},
		},
	}

	if err := p.ValidateRequest(req, config.ModelConfig{Provider: "model-studio", ModelID: "qwen3.8-max"}); err != nil {
		t.Fatalf("ValidateRequest() error = %v, want image and thinking blocks accepted", err)
	}
}

func TestModelStudioProvider_Endpoint(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{"full url kept as-is", "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1/chat/completions", "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1/chat/completions"},
		{"base url gets suffix", "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", "https://dashscope-intl.aliyuncs.com/compatible-mode/v1/chat/completions"},
		{"trailing slash trimmed", "https://dashscope-intl.aliyuncs.com/compatible-mode/v1/", "https://dashscope-intl.aliyuncs.com/compatible-mode/v1/chat/completions"},
		{"empty base url errors", "", ""},
	}

	p := NewModelStudioProvider(nil)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{ModelStudio: config.ModelStudioConfig{BaseURL: tt.baseURL}}
			got, err := p.chatEndpoint(cfg)
			if tt.want == "" {
				if err == nil {
					t.Fatal("chatEndpoint() = nil error, want error for empty base_url")
				}
				return
			}
			if err != nil {
				t.Fatalf("chatEndpoint() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("chatEndpoint() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestModelStudioProvider_AnthropicEndpoint(t *testing.T) {
	const host = "https://token-plan.ap-southeast-1.maas.aliyuncs.com/apps/anthropic"

	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{"apps base gets messages suffix", host, host + "/v1/messages"},
		{"full url kept as-is", host + "/v1/messages", host + "/v1/messages"},
		{"trailing slash trimmed", host + "/", host + "/v1/messages"},
		{"empty base url errors", "", ""},
	}

	p := NewModelStudioProvider(nil)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{ModelStudio: config.ModelStudioConfig{AnthropicBaseURL: tt.baseURL}}
			got, err := p.anthropicEndpoint(cfg)
			if tt.want == "" {
				if err == nil {
					t.Fatal("anthropicEndpoint() = nil error, want error for empty anthropic_base_url")
				}
				return
			}
			if err != nil {
				t.Fatalf("anthropicEndpoint() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("anthropicEndpoint() = %q, want %q", got, tt.want)
			}
		})
	}
}

// On the Anthropic path the upstream already speaks the client's format, so the
// response body must be forwarded byte-for-byte rather than re-serialized.
func TestModelStudioProvider_Execute_AnthropicPassthrough(t *testing.T) {
	// A thinking block: exactly what the OpenAI transform round trip would lose.
	upstream := `{"id":"msg_native","type":"message","role":"assistant","model":"qwen3.8-max",` +
		`"content":[{"type":"thinking","thinking":"hmm","signature":"sig"},` +
		`{"type":"text","text":"done"}],` +
		`"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":2}}`

	var gotPath, gotAuth, gotXAPIKey, gotVersion, gotAccept string
	var gotBody map[string]json.RawMessage
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotXAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotAccept = r.Header.Get("Accept")
		capturedBody, _ = io.ReadAll(r.Body)
		_ = json.Unmarshal(capturedBody, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstream))
	}))
	defer server.Close()

	cfg := &config.Config{ModelStudio: config.ModelStudioConfig{
		BaseURL:          "https://dashscope-intl.aliyuncs.com/compatible-mode/v1",
		AnthropicBaseURL: server.URL,
		APIKey:           "token-plan-key",
	}}
	p := NewModelStudioProvider(config.NewAtomicConfig(cfg, ""))

	req := &core.NormalizedRequest{
		Model:    "qwen3.8-max",
		Messages: []core.NormalizedMessage{{Role: "user", Blocks: []core.NormalizedContentBlock{{Type: "text", Text: "Hi"}}}},
	}
	model := config.ModelConfig{
		Provider:    "model-studio",
		ModelID:     "qwen3.8-max",
		Temperature: 0.3,
		MaxTokens:   4096,
	}

	result, err := p.Execute(context.Background(), req, model)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if string(result.Body) != upstream {
		t.Errorf("body was re-serialized, want verbatim passthrough\n got: %s\nwant: %s", result.Body, upstream)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("path = %q, want %q", gotPath, "/v1/messages")
	}
	if gotAuth != "Bearer token-plan-key" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer token-plan-key")
	}
	if gotXAPIKey != "token-plan-key" {
		t.Errorf("x-api-key = %q, want %q", gotXAPIKey, "token-plan-key")
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want %q", gotVersion, "2023-06-01")
	}
	if gotAccept != "" {
		t.Errorf("Accept = %q, want empty for a non-streaming call", gotAccept)
	}

	// The body must be Anthropic-shaped, not Chat Completions.
	var anthropicReq types.MessageRequest
	if err := json.Unmarshal(capturedBody, &anthropicReq); err != nil {
		t.Fatalf("request body is not a valid Anthropic MessageRequest: %v", err)
	}
	if anthropicReq.Model != "qwen3.8-max" {
		t.Errorf("model = %q, want %q", anthropicReq.Model, "qwen3.8-max")
	}
	if len(anthropicReq.Messages) != 1 || anthropicReq.Messages[0].Role != "user" {
		t.Errorf("messages = %+v, want one user message", anthropicReq.Messages)
	}
	if _, ok := gotBody["stream_options"]; ok {
		t.Error("Chat Completions-only 'stream_options' leaked into the Anthropic request")
	}

	// NormalizedToAnthropic copies only the model ID, so the provider must apply
	// the per-model config itself — otherwise temperature and max_tokens that the
	// Chat Completions path honors would be silently dropped on this one.
	if anthropicReq.Temperature == nil || *anthropicReq.Temperature != 0.3 {
		t.Errorf("temperature = %v, want 0.3 from the model config", anthropicReq.Temperature)
	}
	if anthropicReq.MaxTokens != 4096 {
		t.Errorf("max_tokens = %d, want 4096 from the model config", anthropicReq.MaxTokens)
	}
}

func TestModelStudioProvider_Stream_AnthropicPassthrough(t *testing.T) {
	// Upstream SSE with a thinking delta — the shape the OpenAI transform would
	// have to re-synthesize, and which the passthrough must forward untouched.
	sse := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	var gotAccept, gotPath string
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		gotPath = r.URL.Path
		capturedBody, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer server.Close()

	cfg := &config.Config{ModelStudio: config.ModelStudioConfig{
		AnthropicBaseURL: server.URL,
		APIKey:           "k",
	}}
	p := NewModelStudioProvider(config.NewAtomicConfig(cfg, ""))

	req := &core.NormalizedRequest{
		Model:    "qwen3.8-flash",
		Stream:   true,
		Messages: []core.NormalizedMessage{{Role: "user", Blocks: []core.NormalizedContentBlock{{Type: "text", Text: "Hi"}}}},
	}
	model := config.ModelConfig{Provider: "model-studio", ModelID: "qwen3.8-flash", MaxTokens: 2048}

	if got := p.WireFormat(model); got != core.WireFormatAnthropic {
		t.Fatalf("WireFormat() = %v, want WireFormatAnthropic", got)
	}

	body, err := p.Stream(context.Background(), req, model)
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer func() { _ = body.Close() }()

	if gotAccept != "text/event-stream" {
		t.Errorf("Accept = %q, want %q", gotAccept, "text/event-stream")
	}
	if gotPath != "/v1/messages" {
		t.Errorf("path = %q, want %q", gotPath, "/v1/messages")
	}

	// Reading the stream is the point of the test: without it nothing asserts
	// that the bytes reach the caller unmodified.
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("reading stream: %v", err)
	}
	if string(got) != sse {
		t.Errorf("stream was rewritten, want verbatim passthrough\n got: %q\nwant: %q", got, sse)
	}

	// The streaming path applies the same per-model overrides as Execute.
	var anthropicReq types.MessageRequest
	if err := json.Unmarshal(capturedBody, &anthropicReq); err != nil {
		t.Fatalf("request body is not a valid Anthropic MessageRequest: %v", err)
	}
	if anthropicReq.Stream == nil || !*anthropicReq.Stream {
		t.Errorf("stream = %v, want true", anthropicReq.Stream)
	}
	if anthropicReq.MaxTokens != 2048 {
		t.Errorf("max_tokens = %d, want 2048 from the model config", anthropicReq.MaxTokens)
	}
}

func TestModelStudioProvider_StreamIdleTimeout(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.ModelStudioConfig
		want time.Duration
	}{
		{"stream timeout wins", config.ModelStudioConfig{TimeoutMs: 300000, StreamTimeoutMs: 60000}, 60 * time.Second},
		{"falls back to timeout", config.ModelStudioConfig{TimeoutMs: 300000}, 300 * time.Second},
		{"falls back to default", config.ModelStudioConfig{}, 5 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			atomic := config.NewAtomicConfig(&config.Config{ModelStudio: tt.cfg}, "")
			p := NewModelStudioProvider(atomic)
			if got := p.StreamIdleTimeout(config.ModelConfig{}); got != tt.want {
				t.Errorf("StreamIdleTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

// qwen3.7-max is classified as an Anthropic-wire model on the Go provider. That
// classification must not leak into Model Studio, which authenticates with a
// Bearer token against an OpenAI-compatible URL.
func TestModelStudioProvider_Execute_UsesBearerAuthForAnthropicClassifiedModelID(t *testing.T) {
	var gotAuth, gotXAPIKey, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotXAPIKey = r.Header.Get("x-api-key")
		gotPath = r.URL.Path

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(types.ChatCompletionResponse{
			ID:    "cmpl-test",
			Model: "qwen3.7-max",
			Choices: []types.Choice{{
				Index:        0,
				Message:      types.ChatMessage{Role: "assistant", Content: json.RawMessage(`"ok"`)},
				FinishReason: "stop",
			}},
		})
	}))
	defer server.Close()

	cfg := &config.Config{ModelStudio: config.ModelStudioConfig{
		BaseURL: server.URL,
		APIKey:  "token-plan-key",
	}}
	p := NewModelStudioProvider(config.NewAtomicConfig(cfg, ""))

	req := &core.NormalizedRequest{
		Model:    "qwen3.7-max",
		Messages: []core.NormalizedMessage{{Role: "user", Blocks: []core.NormalizedContentBlock{{Type: "text", Text: "Hi"}}}},
	}
	model := config.ModelConfig{Provider: "model-studio", ModelID: "qwen3.7-max"}

	if _, err := p.Execute(context.Background(), req, model); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if gotAuth != "Bearer token-plan-key" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer token-plan-key")
	}
	if gotXAPIKey != "" {
		t.Errorf("x-api-key = %q, want empty (Model Studio uses Bearer auth)", gotXAPIKey)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want %q", gotPath, "/chat/completions")
	}
}

// DashScope reads enable_thinking at the top level of the request body, not
// nested under `thinking`, so extra_body must be merged in rather than wrapped.
func TestModelStudioProvider_Execute_SendsExtraBodyAtTopLevel(t *testing.T) {
	var rawBody map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&rawBody)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(types.ChatCompletionResponse{
			ID:      "cmpl-test",
			Model:   "qwen3.8-flash",
			Choices: []types.Choice{{Index: 0, Message: types.ChatMessage{Role: "assistant", Content: json.RawMessage(`"ok"`)}, FinishReason: "stop"}},
		})
	}))
	defer server.Close()

	cfg := &config.Config{ModelStudio: config.ModelStudioConfig{BaseURL: server.URL, APIKey: "k"}}
	p := NewModelStudioProvider(config.NewAtomicConfig(cfg, ""))

	req := &core.NormalizedRequest{
		Model:    "qwen3.8-flash",
		Messages: []core.NormalizedMessage{{Role: "user", Blocks: []core.NormalizedContentBlock{{Type: "text", Text: "Hi"}}}},
	}
	model := config.ModelConfig{
		Provider:  "model-studio",
		ModelID:   "qwen3.8-flash",
		ExtraBody: map[string]json.RawMessage{"enable_thinking": json.RawMessage("false")},
	}

	if _, err := p.Execute(context.Background(), req, model); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got, ok := rawBody["enable_thinking"]
	if !ok {
		t.Fatalf("enable_thinking missing from request body; keys = %v", keysOf(rawBody))
	}
	if string(got) != "false" {
		t.Errorf("enable_thinking = %s, want false", got)
	}
	if _, ok := rawBody["model"]; !ok {
		t.Error("declared field 'model' was dropped by the extra_body merge")
	}
	if _, ok := rawBody["messages"]; !ok {
		t.Error("declared field 'messages' was dropped by the extra_body merge")
	}
	if _, ok := rawBody["extra_body"]; ok {
		t.Error("extra_body leaked into the request as its own key; it must be merged")
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestApplyModelStudioOverrides_SkippedWhenThinkingEnabled(t *testing.T) {
	temp := 1.0
	req := &types.MessageRequest{
		MaxTokens:   32000,
		Temperature: &temp,
		Thinking:    json.RawMessage(`{"type":"enabled","budget_tokens":31999}`),
	}
	applyModelStudioOverrides(req, config.ModelConfig{Temperature: 0.7, MaxTokens: 8192})

	if req.MaxTokens != 32000 {
		t.Errorf("MaxTokens = %d, want the client's 32000 (config would drop it below the thinking budget)", req.MaxTokens)
	}
	if req.Temperature == nil || *req.Temperature != 1.0 {
		t.Errorf("Temperature = %v, want the client's 1.0", req.Temperature)
	}
}
