package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/routatic/proxy/internal/client"
	"github.com/routatic/proxy/internal/config"
	"github.com/routatic/proxy/internal/core"
	"github.com/routatic/proxy/internal/transformer"
	"github.com/routatic/proxy/pkg/types"
)

const (
	// chatCompletionsSuffix is the path Model Studio's OpenAI-compatible API
	// expects. base_url may be given with or without it.
	chatCompletionsSuffix = "/chat/completions"

	// anthropicMessagesSuffix is the only path Model Studio's Anthropic-compatible
	// endpoint serves. anthropic_base_url may be given with or without it.
	anthropicMessagesSuffix = "/v1/messages"

	// anthropicVersion is required by the Anthropic protocol. Token Plan accepts
	// requests without it, but sending it keeps the provider correct if the
	// gateway ever enforces the header.
	anthropicVersion = "2023-06-01"
)

// ModelStudioProvider implements core.Provider for Alibaba Cloud Model Studio
// (Bailian / DashScope), including the Token Plan gateway.
//
// Two wire formats are supported:
//
//   - Anthropic Messages, via model_studio.anthropic_base_url. Token Plan serves
//     this natively at /apps/anthropic, so the request and response pass through
//     untransformed — tool use, thinking blocks and cache_control survive intact.
//     This is the preferred path and the default whenever the URL is configured.
//   - OpenAI Chat Completions, via model_studio.base_url. The fallback, and the
//     only option when no Anthropic endpoint is configured.
//
// Model Studio has no Responses or Gemini endpoint, so those wire_format values
// are ignored rather than routed to a URL shape that does not exist.
type ModelStudioProvider struct {
	baseProvider

	// extraBodyWarned holds model IDs already warned about, so the dropped
	// extra_body is logged once per model rather than on every request.
	extraBodyWarned sync.Map
}

// NewModelStudioProvider creates a new ModelStudioProvider.
func NewModelStudioProvider(atomic *config.AtomicConfig) *ModelStudioProvider {
	return &ModelStudioProvider{baseProvider: newBaseProvider(atomic)}
}

// Name returns the provider identifier.
func (p *ModelStudioProvider) Name() string { return config.ProviderModelStudio }

// ValidateRequest checks ordered content against this provider's capabilities
// before any upstream request is attempted.
func (p *ModelStudioProvider) ValidateRequest(req *core.NormalizedRequest, model config.ModelConfig) error {
	return validateRequest(p, req, model)
}

// Capabilities returns provider-level capabilities. Model Studio hosts many
// model families with differing limits, so these are the provider-wide
// ceiling; per-model limits come from the model registry or explicit config.
func (p *ModelStudioProvider) Capabilities() core.ProviderCapabilities {
	return core.ProviderCapabilities{
		SupportsStreaming:  true,
		SupportsTools:      true,
		SupportsThinking:   true,
		SupportsImageInput: true,
		MaxContextLength:   1_000_000,
		DefaultMaxTokens:   8192,
	}
}

// ModelCapabilities returns per-model capabilities. Model Studio hosts many
// families and its catalog is not mirrored here, so every model is accepted;
// vision and context limits are enforced upstream by the capacity filter using
// the model's own config.
func (p *ModelStudioProvider) ModelCapabilities(modelID string) (core.ProviderCapabilities, bool) {
	return p.Capabilities(), true
}

// WireFormat resolves the format for a model. An explicit per-model
// `wire_format` override wins, then `anthropic_tools_disabled`, then the
// configured endpoints: native Anthropic passthrough is preferred whenever
// anthropic_base_url is set, because it avoids the lossy
// Anthropic -> OpenAI -> Anthropic round trip.
func (p *ModelStudioProvider) WireFormat(model config.ModelConfig) core.WireFormat {
	cfg := p.atomic.Get()
	hasAnthropic := strings.TrimSpace(cfg.ModelStudio.AnthropicBaseURL) != ""

	// Responses and Gemini parse successfully but are not served here, so they
	// fall through to the default rather than being honored.
	if wf, ok := core.ParseWireFormat(model.WireFormat); ok {
		switch wf {
		case core.WireFormatAnthropic:
			if hasAnthropic {
				return core.WireFormatAnthropic
			}
		case core.WireFormatOpenAIChat:
			return core.WireFormatOpenAIChat
		}
	}

	if model.AnthropicToolsDisabled {
		return core.WireFormatOpenAIChat
	}
	if hasAnthropic {
		return core.WireFormatAnthropic
	}
	return core.WireFormatOpenAIChat
}

// RoundTripName returns the model ID to use in the upstream request.
func (p *ModelStudioProvider) RoundTripName(model config.ModelConfig) string {
	return model.ModelID
}

// StreamIdleTimeout returns the maximum gap between bytes on an active stream.
func (p *ModelStudioProvider) StreamIdleTimeout(model config.ModelConfig) time.Duration {
	const fallback = 5 * time.Minute
	cfg := p.atomic.Get()
	ms := cfg.ModelStudio.StreamTimeoutMs
	if ms <= 0 {
		ms = cfg.ModelStudio.TimeoutMs
	}
	if ms <= 0 {
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}

// Execute sends a non-streaming request and returns the response.
func (p *ModelStudioProvider) Execute(ctx context.Context, req *core.NormalizedRequest, model config.ModelConfig) (*core.ExecuteResult, error) {
	if p.WireFormat(model) == core.WireFormatAnthropic {
		return p.executeAnthropic(ctx, req, model)
	}
	return p.executeOpenAI(ctx, req, model)
}

// Stream sends a streaming request and returns an io.ReadCloser for SSE events.
func (p *ModelStudioProvider) Stream(ctx context.Context, req *core.NormalizedRequest, model config.ModelConfig) (io.ReadCloser, error) {
	if p.WireFormat(model) == core.WireFormatAnthropic {
		return p.streamAnthropic(ctx, req, model)
	}
	return p.streamOpenAI(ctx, req, model)
}

// ── Anthropic Messages (native passthrough) ────────────────────────────

// warnExtraBodyIgnored logs when a model's extra_body cannot be applied, since
// NormalizedToAnthropic has no place to put it and the setting would otherwise
// vanish without any signal. Logged once per model.
func (p *ModelStudioProvider) warnExtraBodyIgnored(model config.ModelConfig) {
	if len(model.ExtraBody) == 0 {
		return
	}
	if _, seen := p.extraBodyWarned.LoadOrStore(model.ModelID, struct{}{}); !seen {
		slog.Warn("extra_body is ignored on the Anthropic wire format; set wire_format to \"openai\" to apply it",
			"model", model.ModelID)
	}
}

func (p *ModelStudioProvider) executeAnthropic(ctx context.Context, req *core.NormalizedRequest, model config.ModelConfig) (*core.ExecuteResult, error) {
	cfg := p.atomic.Get()
	endpoint, err := p.anthropicEndpoint(cfg)
	if err != nil {
		return nil, err
	}

	anthropicReq := transformer.NormalizedToAnthropic(req, model)
	applyModelStudioOverrides(anthropicReq, model)
	p.warnExtraBodyIgnored(model)
	start := time.Now()
	resp, err := p.doRequest(ctx, endpoint, p.apiKey(cfg), anthropicReq, false, core.WireFormatAnthropic)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// The upstream already speaks Anthropic, so the body is returned untouched.
	return &core.ExecuteResult{
		Body:    body,
		ModelID: model.ModelID,
		Latency: time.Since(start),
	}, nil
}

func (p *ModelStudioProvider) streamAnthropic(ctx context.Context, req *core.NormalizedRequest, model config.ModelConfig) (io.ReadCloser, error) {
	cfg := p.atomic.Get()
	endpoint, err := p.anthropicEndpoint(cfg)
	if err != nil {
		return nil, err
	}

	anthropicReq := transformer.NormalizedToAnthropic(req, model)
	applyModelStudioOverrides(anthropicReq, model)
	p.warnExtraBodyIgnored(model)
	resp, err := p.doRequest(ctx, endpoint, p.apiKey(cfg), anthropicReq, true, core.WireFormatAnthropic)
	if err != nil {
		return nil, err
	}

	return resp.Body, nil
}

// ── OpenAI Chat Completions ────────────────────────────────────────────

func (p *ModelStudioProvider) executeOpenAI(ctx context.Context, req *core.NormalizedRequest, model config.ModelConfig) (*core.ExecuteResult, error) {
	cfg := p.atomic.Get()
	endpoint, err := p.chatEndpoint(cfg)
	if err != nil {
		return nil, err
	}

	openaiReq := transformer.TransformRequestFromNormalized(req, model)
	streamFalse := false
	openaiReq.Stream = &streamFalse

	start := time.Now()
	resp, err := p.doRequest(ctx, endpoint, p.apiKey(cfg), openaiReq, false, core.WireFormatOpenAIChat)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var chatResp types.ChatCompletionResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	normResp := transformer.OpenAIResponseToNormalized(&chatResp, model.ModelID)
	anthropicResp := core.DenormalizeResponse(normResp)
	resultBody, err := json.Marshal(anthropicResp)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal response: %w", err)
	}

	return &core.ExecuteResult{
		Body:    resultBody,
		ModelID: model.ModelID,
		Latency: time.Since(start),
	}, nil
}

func (p *ModelStudioProvider) streamOpenAI(ctx context.Context, req *core.NormalizedRequest, model config.ModelConfig) (io.ReadCloser, error) {
	cfg := p.atomic.Get()
	endpoint, err := p.chatEndpoint(cfg)
	if err != nil {
		return nil, err
	}

	openaiReq := transformer.TransformRequestFromNormalized(req, model)
	streamTrue := true
	openaiReq.Stream = &streamTrue

	resp, err := p.doRequest(ctx, endpoint, p.apiKey(cfg), openaiReq, true, core.WireFormatOpenAIChat)
	if err != nil {
		return nil, err
	}

	return resp.Body, nil
}

// ── Helpers ────────────────────────────────────────────────────────────

// chatEndpoint resolves the Chat Completions URL. The Model Studio docs quote
// the base URL without the /chat/completions suffix, so both spellings are
// accepted.
func (p *ModelStudioProvider) chatEndpoint(cfg *config.Config) (string, error) {
	return withSuffix(cfg.ModelStudio.BaseURL, chatCompletionsSuffix, "model_studio.base_url")
}

// anthropicEndpoint resolves the Anthropic Messages URL, accepting either the
// /apps/anthropic base quoted in Alibaba's docs or the full /v1/messages URL.
func (p *ModelStudioProvider) anthropicEndpoint(cfg *config.Config) (string, error) {
	return withSuffix(cfg.ModelStudio.AnthropicBaseURL, anthropicMessagesSuffix, "model_studio.anthropic_base_url")
}

func withSuffix(raw, suffix, field string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	if base == "" {
		return "", fmt.Errorf("%s is not configured", field)
	}
	if strings.HasSuffix(base, suffix) {
		return base, nil
	}
	return base + suffix, nil
}

// applyModelStudioOverrides applies the per-model config that the Chat
// Completions path applies inside transformer.TransformRequest.
// NormalizedToAnthropic copies only the model ID, so without this the
// temperature and max_tokens a config declares would be silently ignored on the
// path that is the default whenever anthropic_base_url is set — the two wire
// formats would disagree about the same config.
//
// When the client enabled extended thinking the overrides are skipped: Anthropic
// validates max_tokens against the thinking budget and requires temperature 1,
// so a config value could turn a valid request into a 400.
func applyModelStudioOverrides(req *types.MessageRequest, model config.ModelConfig) {
	if len(req.Thinking) > 0 {
		return
	}
	if model.Temperature > 0 {
		temp := model.Temperature
		req.Temperature = &temp
	}
	if model.MaxTokens > 0 {
		req.MaxTokens = model.MaxTokens
	}
}

// apiKey returns the Model Studio key pool in round-robin order, falling back
// to the global keys for backward compatibility.
func (p *ModelStudioProvider) apiKey(cfg *config.Config) string {
	if keys := cfg.ModelStudio.EffectiveAPIKeys(); len(keys) > 0 {
		return p.nextAPIKey(keys)
	}
	return p.nextAPIKey(cfg.EffectiveAPIKeys())
}

func (p *ModelStudioProvider) doRequest(ctx context.Context, url, apiKey string, req any, stream bool, wf core.WireFormat) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", routaticUserAgent)
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	if wf == core.WireFormatAnthropic {
		// Token Plan accepts either credential header; both are sent to match the
		// Go provider's Anthropic path.
		httpReq.Header.Set("x-api-key", apiKey)
		httpReq.Header.Set("anthropic-version", anthropicVersion)
	}
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		bodyBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, &client.APIError{StatusCode: resp.StatusCode, Body: string(bodyBytes)}
	}

	return resp, nil
}
