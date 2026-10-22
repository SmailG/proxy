package router

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/routatic/proxy/internal/config"
)

// --- shadow: inputs that must not reach Jev ----------------------------------

func TestJevRouting_SkipsWhenLatestUserMessageHasNoAuthoredText(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(2, 0.9)))
	for _, mode := range []string{config.JevModeShadow, config.JevModeActive} {
		r, logs := newJevRouter(t, stub, mode)
		// A tool-result turn: Content holds tool output, PromptText is empty.
		msgs := []MessageContent{{Role: "user", Content: "file contents: refactor everything"}}
		got := route(t, r, msgs, 100, 0)
		r.jev.wg.Wait()
		if got.Scenario != ScenarioComplex { // the keyword result, from Content
			t.Errorf("%s: scenario = %s, want keyword result complex", mode, got.Scenario)
		}
		wantLog(t, logs, "no authored text")
	}
	if n := stub.hits.Load(); n != 0 {
		t.Errorf("Jev was called %d times for a turn with no authored text", n)
	}
}

func TestJevRouting_SendsPromptTextNeverToolOutput(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(1, 0.1)))
	r, _ := newJevRouter(t, stub, config.JevModeActive)
	msgs := []MessageContent{
		{Role: "system", Content: "SYSTEM PROMPT TEXT", PromptText: "SYSTEM PROMPT TEXT"},
		{Role: "user", Content: "older question", PromptText: "older question"},
		{Role: "user", Content: "hello [tool output: SECRET]", PromptText: "hello"},
	}
	route(t, r, msgs, 100, 0)
	body := string(stub.last.Load().body)
	if !strings.Contains(body, `"state":{"request":"hello"}`) {
		t.Errorf("state is not the latest user PromptText: %s", body)
	}
	for _, leaked := range []string{"SECRET", "SYSTEM PROMPT TEXT", "older question"} {
		if strings.Contains(body, leaked) {
			t.Errorf("request body leaks %q: %s", leaked, body)
		}
	}
}

// --- active: mapping --------------------------------------------------------

func TestJevRouting_ActiveMapsLevelsToScenarios(t *testing.T) {
	cases := []struct {
		name  string
		score float64
		text  string
		want  Scenario
	}{
		{"level 2 is complex", 2, "Hello", ScenarioComplex},
		{"level 1 is default", 1, "Hello", ScenarioDefault},
		{"level 0 is background", 0, "Hello", ScenarioBackground},
		// "edit" is a tool keyword: the existing background precondition fails.
		{"level 0 with a tool keyword stays default", 0, "show me the file, don't edit it", ScenarioDefault},
		// Jev replaces the keyword decision, in both directions.
		{"level 1 overrides keyword complex", 1, negationText, ScenarioDefault},
		{"level 2 overrides keyword default", 2, "Hello", ScenarioComplex},
		{"level 1 overrides keyword think", 1, "Think about this", ScenarioDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newJevStub(t, respondJSON(jevOKBody(tc.score, 0.2)))
			r, logs := newJevRouter(t, stub, config.JevModeActive)
			got := route(t, r, userMsg(tc.text), 100, 0)
			if got.Scenario != tc.want {
				t.Errorf("scenario = %s, want %s", got.Scenario, tc.want)
			}
			wantLog(t, logs, "mode=active", "jev_scenario="+string(tc.want))
			if n := stub.hits.Load(); n != 1 {
				t.Errorf("Jev requests = %d, want exactly 1 per request", n)
			}
		})
	}
}

func TestJevRouting_ActiveReasonNamesNoModel(t *testing.T) {
	banned := []string{"glm", "kimi", "qwen", "minimax", "mimo", "deepseek", "jev-1", "m-complex", "m-default", "m-background"}
	for _, score := range []float64{0, 1, 2} {
		stub := newJevStub(t, respondJSON(jevOKBody(score, 0.2)))
		r, _ := newJevRouter(t, stub, config.JevModeActive)
		reason := strings.ToLower(route(t, r, userMsg("Hello"), 100, 0).Reason)
		// RouteResult.Reason ends with the model the router resolved; the part
		// before it is the scenario Reason under test.
		scenarioPart, _, _ := strings.Cut(reason, " -> ")
		if !strings.Contains(scenarioPart, "semantic classifier") {
			t.Fatalf("level %v: reason %q does not say the classifier decided", score, reason)
		}
		for _, m := range banned {
			if strings.Contains(scenarioPart, m) {
				t.Errorf("level %v: scenario reason names model %q: %s", score, m, scenarioPart)
			}
		}
	}
}

// --- active: fail-open -------------------------------------------------------

func TestJevRouting_ActiveFailsOpenToKeywordResult(t *testing.T) {
	status := func(code int) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"detail":"` + jevTestKey + `"}`)) // hostile body echoing the key
		}
	}
	cases := []struct {
		name   string
		serve  func(http.ResponseWriter, *http.Request)
		reason string
	}{
		{"http 429", status(429), "http status 429"},
		{"http 500", status(500), "http status 500"},
		{"malformed json", respondJSON(`{"answers":`), "malformed"},
		{"score out of range", respondJSON(jevOKBody(5, 0.1)), "out of range"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newJevStub(t, tc.serve)
			r, logs := newJevRouter(t, stub, config.JevModeActive)
			got := route(t, r, userMsg(negationText), 100, 0)
			if got.Scenario != ScenarioComplex {
				t.Errorf("scenario = %s, want the keyword result complex", got.Scenario)
			}
			wantLog(t, logs, "keyword scenario stands", tc.reason)
		})
	}
}

func TestJevRouting_ActiveMissingKeyFailsOpen(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(1, 0.1)))
	r, logs := newJevRouter(t, stub, config.JevModeActive)
	t.Setenv(jevTestKeyEnv, "")
	got := route(t, r, userMsg(negationText), 100, 0)
	if got.Scenario != ScenarioComplex {
		t.Errorf("scenario = %s, want the keyword result complex", got.Scenario)
	}
	wantLog(t, logs, "keyword scenario stands", "api key environment variable")
	if n := stub.hits.Load(); n != 0 {
		t.Errorf("a request was sent without a key (%d)", n)
	}
}

// Active mode waits for Jev, but never longer than timeout_ms.
func TestJevRouting_ActiveWaitIsBoundedByTimeout(t *testing.T) {
	release := make(chan struct{})
	stub := newJevStub(t, func(_ http.ResponseWriter, req *http.Request) {
		select {
		case <-release:
		case <-req.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	r, logs := newJevRouter(t, stub, config.JevModeActive, func(c *config.JevRoutingConfig) { c.TimeoutMs = 60 })
	start := time.Now()
	got := route(t, r, userMsg(negationText), 100, 0)
	elapsed := time.Since(start)
	if got.Scenario != ScenarioComplex {
		t.Errorf("scenario = %s, want the keyword result complex", got.Scenario)
	}
	if elapsed < 50*time.Millisecond || elapsed > time.Second {
		t.Errorf("waited %v, want about the 60ms timeout", elapsed)
	}
	wantLog(t, logs, "keyword scenario stands", "deadline exceeded")
}

// --- streaming --------------------------------------------------------------

func TestJevRouting_StreamingPathIsUnchanged(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(0, 0.1)))
	r, _ := newJevRouter(t, stub, config.JevModeActive)
	got, err := r.RouteForStreaming(userMsg(negationText), 100, "")
	if err != nil {
		t.Fatalf("RouteForStreaming: %v", err)
	}
	r.jev.wg.Wait()
	if got.Scenario != ScenarioFast {
		t.Errorf("scenario = %s, want fast (complex/think downgrade)", got.Scenario)
	}
	if n := stub.hits.Load(); n != 0 {
		t.Errorf("streaming routing called Jev %d times", n)
	}
}

// --- hot reload -------------------------------------------------------------

func TestJevRouting_ModeChangeTakesEffectWithoutNewRouter(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(1, 0.1)))
	r, _ := newJevRouter(t, stub, config.JevModeShadow)
	if got := route(t, r, userMsg(negationText), 100, 0); got.Scenario != ScenarioComplex {
		t.Fatalf("shadow: scenario = %s", got.Scenario)
	}
	r.jev.wg.Wait()
	next := *r.atomic.Get()
	next.JevRouting.Mode = config.JevModeActive
	r.atomic = newTestAtomicConfig(&next)
	if got := route(t, r, userMsg(negationText), 100, 0); got.Scenario != ScenarioDefault {
		t.Errorf("active after reload: scenario = %s, want default", got.Scenario)
	}
}
