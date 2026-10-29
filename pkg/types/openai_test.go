package types

import (
	"encoding/json"
	"testing"
)

func TestChatCompletionRequest_MarshalJSON_WithoutExtraBody(t *testing.T) {
	stream := true
	req := ChatCompletionRequest{Model: "qwen3.8-max", Stream: &stream}

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["model"]; !ok {
		t.Error("model missing from marshaled request")
	}
	if _, ok := got["extra_body"]; ok {
		t.Error("extra_body key emitted although ExtraBody is empty")
	}
}

func TestChatCompletionRequest_MarshalJSON_MergesExtraBody(t *testing.T) {
	stream := false
	req := ChatCompletionRequest{
		Model:  "qwen3.8-flash",
		Stream: &stream,
		ExtraBody: map[string]json.RawMessage{
			"enable_thinking": json.RawMessage("false"),
			"top_k":           json.RawMessage("20"),
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for key, want := range map[string]string{
		"enable_thinking": "false",
		"top_k":           "20",
	} {
		v, ok := got[key]
		if !ok {
			t.Errorf("%s missing from marshaled request; body = %s", key, body)
			continue
		}
		if string(v) != want {
			t.Errorf("%s = %s, want %s", key, v, want)
		}
	}

	// Declared fields survive the merge.
	for _, key := range []string{"model", "stream"} {
		if _, ok := got[key]; !ok {
			t.Errorf("declared field %q dropped by the merge; body = %s", key, body)
		}
	}
	if _, ok := got["extra_body"]; ok {
		t.Errorf("extra_body emitted as its own key; body = %s", body)
	}
}

// ExtraBody is the escape hatch for vendor params, so a collision must resolve
// in its favour rather than silently dropping the caller's value.
func TestChatCompletionRequest_MarshalJSON_ExtraBodyWinsCollision(t *testing.T) {
	req := ChatCompletionRequest{
		Model:     "qwen3.8-max",
		ExtraBody: map[string]json.RawMessage{"model": json.RawMessage(`"override"`)},
	}

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(got["model"]) != `"override"` {
		t.Errorf("model = %s, want %q", got["model"], `"override"`)
	}
}

// A pointer to the request is what the providers marshal; the value-receiver
// method must still be in its method set.
func TestChatCompletionRequest_MarshalJSON_PointerReceiver(t *testing.T) {
	req := &ChatCompletionRequest{
		Model:     "glm-5.3",
		ExtraBody: map[string]json.RawMessage{"enable_thinking": json.RawMessage("true")},
	}

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(got["enable_thinking"]) != "true" {
		t.Errorf("enable_thinking = %s, want true", got["enable_thinking"])
	}
}
