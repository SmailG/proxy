package transformer

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/routatic/proxy/pkg/types"
)

// Anthropic's own rule for tool_use.id.
var anthropicToolID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func TestSanitizeToolID(t *testing.T) {
	cases := map[string]string{
		"read_file:0":                  "read_file_0", // kimi-k3
		"functions.Read:3":             "functions_Read_3",
		"call_00_oDF2a2zq3biyDlfPQAV0": "call_00_oDF2a2zq3biyDlfPQAV0",
		"chatcmpl-tool-ba5e97feb675":   "chatcmpl-tool-ba5e97feb675",
		"a b/é":                        "a_b___", // one '_' per byte
	}
	for in, want := range cases {
		if got := sanitizeToolID(in); got != want {
			t.Errorf("sanitizeToolID(%q) = %q, want %q", in, got, want)
		}
	}
	if got := sanitizeToolID(""); !strings.HasPrefix(got, "toolu_") || !anthropicToolID.MatchString(got) {
		t.Errorf("empty id -> %q, want a generated toolu_ id", got)
	}
}

func TestProxyStream_ToolUseIDMatchesAnthropicPattern(t *testing.T) {
	handler := NewStreamHandler()
	w := newMockResponseWriter()
	body := sseLines(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"read_file:0","type":"function","function":{"name":"read_file","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := handler.ProxyStream(w, body, "kimi-k3", ctx, 0, cancel); err != nil {
		t.Fatalf("ProxyStream error: %v", err)
	}
	var got string
	for _, e := range parseSSEEvents(t, w.buf.String()) {
		if e.ContentBlock != nil && e.ContentBlock.Type == "tool_use" {
			got = e.ContentBlock.ID
		}
	}
	if got != "read_file_0" {
		t.Errorf("streamed tool_use.id = %q, want read_file_0", got)
	}
}

func TestTransformResponse_ToolUseIDMatchesAnthropicPattern(t *testing.T) {
	resp := &types.ChatCompletionResponse{
		Model: "kimi-k3",
		Choices: []types.Choice{{
			Message: types.ChatMessage{
				Role:    "assistant",
				Content: contentText(""),
				ToolCalls: []types.ToolCall{{
					ID: "read_file:0", Type: "function",
					Function: types.FunctionCall{Name: "read_file", Arguments: `{}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}
	out, err := NewResponseTransformer().TransformResponse(resp, "kimi-k3")
	if err != nil {
		t.Fatalf("TransformResponse: %v", err)
	}
	found := false
	for _, b := range out.Content {
		if b.Type == "tool_use" {
			found = true
			if !anthropicToolID.MatchString(b.ID) {
				t.Errorf("tool_use.id = %q does not match ^[a-zA-Z0-9_-]+$", b.ID)
			}
		}
	}
	if !found {
		t.Fatal("no tool_use block in the response")
	}
}

func TestResponsesPaths_ToolUseIDMatchesAnthropicPattern(t *testing.T) {
	const callID = "fc:0.x"

	t.Run("stream", func(t *testing.T) {
		handler := NewStreamHandler()
		w := newMockResponseWriter()
		body := sseLines(
			`{"type":"response.output_item.added","output":[{"type":"function_call","id":"fc_1","call_id":"`+callID+`","name":"get_weather","arguments":"","status":"in_progress"}]}`,
			`{"type":"response.output_item.done","output":[{"type":"function_call","id":"fc_1","call_id":"`+callID+`","name":"get_weather","arguments":"{}","status":"completed"}]}`,
			`{"type":"response.completed"}`,
		)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := handler.ProxyResponsesStream(w, body, "muse-spark-1.2-contributor", ctx, 0, cancel); err != nil {
			t.Fatalf("ProxyResponsesStream: %v", err)
		}
		var got string
		for _, e := range parseSSEEvents(t, w.buf.String()) {
			if e.ContentBlock != nil && e.ContentBlock.Type == "tool_use" {
				got = e.ContentBlock.ID
			}
		}
		if got != "fc_0_x" {
			t.Errorf("tool_use.id = %q, want fc_0_x", got)
		}
	})

	t.Run("non-stream", func(t *testing.T) {
		var r types.ResponsesResponse
		raw := `{"id":"r","model":"m","output":[{"type":"function_call","call_id":"` + callID + `","name":"get_weather","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":1}}`
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatal(err)
		}
		out, err := NewResponseTransformer().TransformResponsesResponse(&r, r.Model)
		if err != nil {
			t.Fatalf("TransformResponsesResponse: %v", err)
		}
		if len(out.Content) == 0 || out.Content[0].ID != "fc_0_x" {
			t.Errorf("content = %+v, want tool_use id fc_0_x", out.Content)
		}
	})

	t.Run("normalized bridge", func(t *testing.T) {
		var r types.ResponsesResponse
		raw := `{"id":"r","model":"m","output":[{"type":"function_call","call_id":"` + callID + `","name":"get_weather","arguments":"{}"}]}`
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatal(err)
		}
		nr := ResponsesToNormalized(&r, "m")
		if got := nr.Messages[0].Blocks[0].ID; got != "fc_0_x" {
			t.Errorf("responses normalized id = %q, want fc_0_x", got)
		}
		cr := &types.ChatCompletionResponse{Choices: []types.Choice{{Message: types.ChatMessage{
			Role: "assistant", Content: contentText(""),
			ToolCalls: []types.ToolCall{{ID: "read_file:0", Type: "function", Function: types.FunctionCall{Name: "f", Arguments: "{}"}}},
		}}}}
		nc := OpenAIResponseToNormalized(cr, "m")
		if got := nc.Messages[0].Blocks[len(nc.Messages[0].Blocks)-1].ID; got != "read_file_0" {
			t.Errorf("chat normalized id = %q, want read_file_0", got)
		}
	})
}
