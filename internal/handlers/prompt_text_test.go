package handlers

import (
	"encoding/json"
	"testing"

	"github.com/routatic/proxy/pkg/types"
)

// extractPromptText feeds the opt-in Jev classifier, which sends text off-machine.
// A Claude Code turn that carries tool output must yield only what the user
// typed, never the tool output.
func TestExtractPromptText_ExcludesToolOutputAndMarkers(t *testing.T) {
	raw := `[
		{"type":"tool_result","tool_use_id":"t1","content":"SECRET FILE CONTENTS"},
		{"type":"tool_use","id":"t2","name":"Bash","input":{}},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
		{"type":"thinking","thinking":"private thoughts"},
		{"type":"text","text":"fix the "},
		{"type":"text","text":"parser"}
	]`
	var blocks []types.ContentBlock
	if err := json.Unmarshal([]byte(raw), &blocks); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := extractPromptText(blocks); got != "fix the parser" {
		t.Errorf("extractPromptText = %q, want %q", got, "fix the parser")
	}
	// Content (used for keyword routing and token counts) keeps the tool output;
	// the two must stay different or the privacy boundary is gone.
	if content := extractTextFromBlocks(blocks); content == extractPromptText(blocks) {
		t.Errorf("Content and PromptText are identical; PromptText must exclude tool output")
	}
}

func TestExtractPromptText_ToolResultOnlyTurnIsEmpty(t *testing.T) {
	blocks := []types.ContentBlock{{Type: "tool_result", Content: json.RawMessage(`"output"`)}}
	if got := extractPromptText(blocks); got != "" {
		t.Errorf("extractPromptText = %q, want empty for a tool_result-only turn", got)
	}
}
