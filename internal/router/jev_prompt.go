package router

import "strings"

// The harness (Claude Code) wraps its own context in <system-reminder> blocks and
// sends them as text blocks of the user turn. They are not authored by the user
// and can carry file paths, memory and settings, so they are removed before the
// text is classified or sent off-machine.
const (
	harnessOpenTag  = "<system-reminder>"
	harnessCloseTag = "</system-reminder>"
)

// stripHarnessBlocks removes every <system-reminder>...</system-reminder> block
// (tag name exact and case-sensitive; blocks may nest) and trims the result.
//
// An opening tag with no matching close fails closed: the tag and everything
// after it are dropped, because what follows may be harness content and the cost
// of dropping real text is only a less informed (or skipped) classification. A
// stray closing tag is left in place. Text with no opening tag is returned
// unchanged.
func stripHarnessBlocks(s string) string {
	if !strings.Contains(s, harnessOpenTag) {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, harnessOpenTag)
		if i < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:i])
		rest := s[i+len(harnessOpenTag):]
		end, ok := matchingCloseEnd(rest)
		if !ok {
			break
		}
		s = rest[end:]
	}
	return strings.TrimSpace(b.String())
}

// matchingCloseEnd returns the offset just past the close tag that balances an
// already-consumed opening tag, counting nested blocks.
func matchingCloseEnd(s string) (int, bool) {
	depth, pos := 1, 0
	for {
		c := strings.Index(s[pos:], harnessCloseTag)
		if c < 0 {
			return 0, false
		}
		if o := strings.Index(s[pos:], harnessOpenTag); o >= 0 && o < c {
			depth++
			pos += o + len(harnessOpenTag)
			continue
		}
		depth--
		pos += c + len(harnessCloseTag)
		if depth == 0 {
			return pos, true
		}
	}
}
