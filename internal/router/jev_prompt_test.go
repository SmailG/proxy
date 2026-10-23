package router

import (
	"strings"
	"testing"

	"github.com/routatic/proxy/internal/config"
)

func TestStripHarnessBlocks(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"no tag is untouched", "  fix the parser ", "  fix the parser "},
		{"reminder only", "<system-reminder>\nbe nice\n</system-reminder>", ""},
		{"reminder then text", "<system-reminder>x</system-reminder>\nfix the parser", "fix the parser"},
		{"text around reminder", "before <system-reminder>x</system-reminder> after", "before  after"},
		{"two reminders", "<system-reminder>a</system-reminder>hi<system-reminder>b</system-reminder>", "hi"},
		{"nested reminders", "<system-reminder>a <system-reminder>b</system-reminder> c</system-reminder>go", "go"},
		{"unterminated drops the rest", "keep this <system-reminder>never closed, may be harness", "keep this"},
		{"unterminated nested drops the rest", "<system-reminder>a <system-reminder>b</system-reminder> c", ""},
		{"stray close tag is left alone", "text </system-reminder> more", "text </system-reminder> more"},
		{"other tags are left alone", "<system-note>x</system-note> hi", "<system-note>x</system-note> hi"},
		{"tag name is exact and case-sensitive", "<System-Reminder>x</System-Reminder> hi", "<System-Reminder>x</System-Reminder> hi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripHarnessBlocks(tc.in); got != tc.want {
				t.Errorf("stripHarnessBlocks(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

const reminderBlock = "<system-reminder>\nAs you answer the user's questions, you can use the following context: SECRET-HARNESS\n</system-reminder>"

func TestJevRouting_ReminderOnlyTurnIsSkipped(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(2, 0.9)))
	for _, mode := range []string{config.JevModeShadow, config.JevModeActive} {
		r, logs := newJevRouter(t, stub, mode)
		msgs := []MessageContent{{Role: "user", Content: reminderBlock, PromptText: reminderBlock}}
		route(t, r, msgs, 100, 0)
		r.jev.wg.Wait()
		wantLog(t, logs, "no authored text")
	}
	if n := stub.hits.Load(); n != 0 {
		t.Errorf("Jev was called %d times for a reminder-only turn", n)
	}
}

func TestJevRouting_SendsOnlyTheAuthoredTextAroundAReminder(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(1, 0.1)))
	r, _ := newJevRouter(t, stub, config.JevModeActive)
	text := reminderBlock + "fix the parser"
	route(t, r, []MessageContent{{Role: "user", Content: text, PromptText: text}}, 100, 0)
	body := string(stub.last.Load().body)
	if !strings.Contains(body, `"state":{"request":"fix the parser"}`) {
		t.Errorf("state is not the authored text alone: %s", body)
	}
	if strings.Contains(body, "SECRET-HARNESS") || strings.Contains(body, "system-reminder") {
		t.Errorf("request body carries the reminder: %s", body)
	}
}

// The 600-character cut happens after stripping: a long reminder must not eat
// the budget, and a long authored text is still cut to 600 characters.
func TestJevRouting_CutAppliesAfterStripping(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(1, 0.1)))
	r, _ := newJevRouter(t, stub, config.JevModeActive)
	long := "<system-reminder>" + strings.Repeat("R", 2000) + "</system-reminder>" + strings.Repeat("a", 700)
	route(t, r, []MessageContent{{Role: "user", Content: long, PromptText: long}}, 100, 0)
	body := string(stub.last.Load().body)
	want := `"state":{"request":"` + strings.Repeat("a", jevRequestMaxChars) + `"}`
	if !strings.Contains(body, want) {
		t.Errorf("sent text is not the first %d authored characters: %.200s", jevRequestMaxChars, body)
	}
}

func TestJevRouting_UnterminatedReminderNeverReachesJev(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(1, 0.1)))
	r, _ := newJevRouter(t, stub, config.JevModeActive)
	text := "fix the parser <system-reminder>cut off SECRET-HARNESS"
	route(t, r, []MessageContent{{Role: "user", Content: text, PromptText: text}}, 100, 0)
	body := string(stub.last.Load().body)
	if !strings.Contains(body, `"state":{"request":"fix the parser"}`) || strings.Contains(body, "SECRET-HARNESS") {
		t.Errorf("unterminated reminder was not dropped to end of text: %s", body)
	}
}

// Streaming shadow strips too.
func TestJevStreamingShadow_StripsReminders(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(1, 0.1)))
	r, _ := newJevRouter(t, stub, config.JevModeShadow)
	text := reminderBlock + "fix the parser"
	routeStream(t, r, []MessageContent{{Role: "user", Content: text, PromptText: text}}, 100)
	r.jev.wg.Wait()
	body := string(stub.last.Load().body)
	if !strings.Contains(body, `"state":{"request":"fix the parser"}`) {
		t.Errorf("streaming state is not the authored text alone: %s", body)
	}
}
