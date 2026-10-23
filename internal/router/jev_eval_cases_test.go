package router

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
)

// evalCase is one labelled request in testdata/jev_eval_cases.jsonl: the text
// and the routing level (0..2) a person would assign it. The file is the input
// for a future offline evaluation of the Jev classifier against a live API; no
// test here sends it anywhere.
type evalCase struct {
	Text   string `json:"text"`
	Level  int    `json:"level"`
	Source string `json:"source"`
}

func loadEvalCases(t *testing.T) []evalCase {
	t.Helper()
	f, err := os.Open("testdata/jev_eval_cases.jsonl")
	if err != nil {
		t.Fatalf("open eval cases: %v", err)
	}
	defer func() { _ = f.Close() }()
	var cases []evalCase
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		var c evalCase
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatalf("line %d: %v", line, err)
		}
		cases = append(cases, c)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return cases
}

// The file must stay well-formed so a future evaluation can trust it.
func TestJevEvalCases_WellFormed(t *testing.T) {
	cases := loadEvalCases(t)
	perLevel := map[int]int{}
	seen := map[string]bool{}
	for i, c := range cases {
		if c.Text == "" || c.Source == "" {
			t.Errorf("case %d: text and source are required: %+v", i, c)
		}
		if c.Level < 0 || c.Level > jevTopLevel {
			t.Errorf("case %d: level %d outside 0..%d", i, c.Level, jevTopLevel)
		}
		if seen[c.Text] {
			t.Errorf("case %d: duplicate text %q", i, c.Text)
		}
		seen[c.Text] = true
		perLevel[c.Level]++
	}
	for level := 0; level <= jevTopLevel; level++ {
		if perLevel[level] < 5 {
			t.Errorf("level %d has %d cases, want at least 5", level, perLevel[level])
		}
	}
}

// Cases labelled "negation" exist because the keyword matcher routes them to a
// scenario that differs from what their level asks for. This keeps that claim
// true: if the matcher is ever fixed, the label must move.
func TestJevEvalCases_NegationCasesAreMisroutedByKeywords(t *testing.T) {
	for _, c := range loadEvalCases(t) {
		if c.Source != "negation" {
			continue
		}
		msgs := userMsg(c.Text)
		want := jevScenarioForLevel(c.Level, hasToolBlocker(msgs))
		if got := DetectScenario(msgs, 100, mockConfig()).Scenario; got == want {
			t.Errorf("%q: keyword routing already gives %s; it is not a negation misfire", c.Text, got)
		}
	}
}
