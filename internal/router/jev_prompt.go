package router

import "strings"

// The harness (Claude Code) wraps its own context in tagged blocks and sends them
// as text blocks of the user turn: <system-reminder> blocks, and the echo of a
// slash command (<command-name>, <command-message>, <command-args>) and its
// output (<local-command-stdout>, <local-command-stderr>, <local-command-caveat>).
// They are not authored by the user and can carry file paths, memory and settings,
// so they are removed before the text is classified or sent off-machine.
//
// A text block that starts with "Called the <X> tool" or "Result of calling the
// <X> tool" is the harness's record of a tool call, not authored text; the handler
// drops those blocks (see isHarnessToolEcho).
var harnessTags = func() []harnessTag {
	names := []string{
		"system-reminder",
		"command-name", "command-message", "command-args",
		"local-command-stdout", "local-command-stderr", "local-command-caveat",
	}
	tags := make([]harnessTag, len(names))
	for i, n := range names {
		tags[i] = harnessTag{open: "<" + n + ">", close: "</" + n + ">"}
	}
	return tags
}()

type harnessTag struct{ open, close string }

// maxJevPromptBytes bounds the authored text Jev will look at. Longer text is not
// classified: stripping and sending a pasted document would cost time and send
// far more than the 600-character cut that follows, and the keyword result
// stands. The check is on the raw length, before any scanning.
const maxJevPromptBytes = 64 * 1024

// stripHarnessBlocks removes every harness block (tag names exact and
// case-sensitive; blocks of the same tag may nest) and trims the result. It is a
// single left-to-right pass: every byte is examined a bounded number of times.
//
// An opening tag with no matching close fails closed: the tag and everything
// after it are dropped, because what follows may be harness content and the cost
// of dropping real text is only a less informed (or skipped) classification. A
// stray closing tag is left in place. Text with no harness opening tag is
// returned unchanged.
func stripHarnessBlocks(s string) string {
	if !hasHarnessOpen(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		j := strings.IndexByte(s[i:], '<')
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+j])
		i += j
		tag, ok := openTagAt(s[i:])
		if !ok {
			b.WriteByte('<')
			i++
			continue
		}
		end, closed := blockEnd(s[i+len(tag.open):], tag)
		if !closed {
			break // fail closed: drop the open tag and the rest
		}
		i += len(tag.open) + end
	}
	return strings.TrimSpace(b.String())
}

func hasHarnessOpen(s string) bool {
	for _, t := range harnessTags {
		if strings.Contains(s, t.open) {
			return true
		}
	}
	return false
}

func openTagAt(s string) (harnessTag, bool) {
	for _, t := range harnessTags {
		if strings.HasPrefix(s, t.open) {
			return t, true
		}
	}
	return harnessTag{}, false
}

// blockEnd returns the offset just past the close tag that balances an
// already-consumed opening tag of the same name, counting nested blocks of that
// name. Other tags inside the block are content. One pass over s.
func blockEnd(s string, tag harnessTag) (int, bool) {
	depth, pos := 1, 0
	for {
		j := strings.IndexByte(s[pos:], '<')
		if j < 0 {
			return 0, false
		}
		pos += j
		switch {
		case strings.HasPrefix(s[pos:], tag.open):
			depth++
			pos += len(tag.open)
		case strings.HasPrefix(s[pos:], tag.close):
			depth--
			pos += len(tag.close)
			if depth == 0 {
				return pos, true
			}
		default:
			pos++
		}
	}
}
