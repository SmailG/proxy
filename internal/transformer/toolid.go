package transformer

import "fmt"

// Anthropic's API accepts a tool_use id only if it matches ^[a-zA-Z0-9_-]+$.
// Upstream models do not follow that: kimi-k3 returns "read_file:0". The id is
// copied into the history Claude Code keeps, and any Anthropic-side check of
// that history (compaction, the advisor tool) rejects it with a 400.

// sanitizeToolID returns id with every character outside [a-zA-Z0-9_-] replaced
// by '_'. An empty id gets a generated toolu_ id. Replies and history carry the
// same result, so a tool_result still pairs with its tool_use.
func sanitizeToolID(id string) string {
	if id == "" {
		return fmt.Sprintf("toolu_%s", generateID())
	}
	out := []byte(id)
	for i, c := range out {
		if !isToolIDChar(c) {
			out[i] = '_'
		}
	}
	return string(out)
}

func isToolIDChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}
