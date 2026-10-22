package router

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/routatic/proxy/internal/config"
)

// Tests for the findings of the independent review of the Jev classifier PR.

func TestStripHarnessBlocks_OtherHarnessTags(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"command echo", "<command-name>/compact</command-name><command-message>compact</command-message><command-args>x</command-args>", ""},
		{"command output", "<local-command-stdout>PRIVATE OUTPUT</local-command-stdout>fix the parser", "fix the parser"},
		{"caveat and stderr", "<local-command-caveat>c</local-command-caveat><local-command-stderr>e</local-command-stderr>hi", "hi"},
		{"different tags do not close each other", "<command-name>a</system-reminder> still inside</command-name>done", "done"},
		{"inner tag of another family is content", "<system-reminder>a <command-name>b</command-name> c</system-reminder>go", "go"},
		{"less-than in prose survives", "if a < b then <ok> fix it", "if a < b then <ok> fix it"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripHarnessBlocks(tc.in); got != tc.want {
				t.Errorf("stripHarnessBlocks(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The first version was quadratic: 40,000 nested opens took 12.8 s. It must now be linear.
func TestStripHarnessBlocks_DeepNestingIsLinear(t *testing.T) {
	const depth = 100_000
	in := strings.Repeat(harnessTags[0].open, depth) + strings.Repeat(harnessTags[0].close, depth) + "keep"
	start := time.Now()
	got := stripHarnessBlocks(in)
	if got != "keep" {
		t.Fatalf("got %q, want %q", got, "keep")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("stripping %d nested blocks took %v; want linear time", depth, d)
	}
}

func TestLatestPromptText_OverTheCapIsNotClassified(t *testing.T) {
	big := strings.Repeat("a", maxJevPromptBytes+1)
	if got := latestPromptText(userMsg(big)); got != "" {
		t.Errorf("text over %d bytes must be skipped, got %d bytes", maxJevPromptBytes, len(got))
	}
	atCap := strings.Repeat("a", maxJevPromptBytes)
	if got := latestPromptText(userMsg(atCap)); len(got) != maxJevPromptBytes {
		t.Errorf("text exactly at the cap must still be classified, got %d bytes", len(got))
	}
	stub := newJevStub(t, respondJSON(jevOKBody(1, 0.1)))
	r, logs := newJevRouter(t, stub, config.JevModeActive)
	route(t, r, userMsg(big), 100, 0)
	if n := stub.hits.Load(); n != 0 {
		t.Errorf("over-cap text reached Jev %d times", n)
	}
	wantLog(t, logs, "no authored text")
}

// Finding 1: the override safety-net chain must never reach Jev.
func TestRouteNoJev_NeverCallsJev(t *testing.T) {
	for _, mode := range []string{config.JevModeShadow, config.JevModeActive} {
		stub := newJevStub(t, respondJSON(jevOKBody(2, 0.9)))
		r, _ := newJevRouter(t, stub, mode)
		msgs := userMsg("Hello there")
		if _, err := r.RouteWithToolsNoJev(msgs, 100, 0, ""); err != nil {
			t.Fatalf("RouteWithToolsNoJev(%s): %v", mode, err)
		}
		if _, err := r.RouteForStreamingNoJev(msgs, 100, ""); err != nil {
			t.Fatalf("RouteForStreamingNoJev(%s): %v", mode, err)
		}
		r.jev.wg.Wait()
		if n := stub.hits.Load(); n != 0 {
			t.Errorf("mode %s: the no-Jev routes reached Jev %d times", mode, n)
		}
	}
}

// Control for the test above: the same calls through the normal entry points do reach
// Jev in shadow mode, so a zero above is not a vacuous pass.
func TestRouteNoJev_ControlNormalRoutesDoReachJev(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(2, 0.9)))
	r, _ := newJevRouter(t, stub, config.JevModeShadow)
	msgs := userMsg("Hello there")
	route(t, r, msgs, 100, 0)
	if _, err := r.RouteForStreaming(msgs, 100, ""); err != nil {
		t.Fatalf("RouteForStreaming: %v", err)
	}
	r.jev.wg.Wait()
	if n := stub.hits.Load(); n != 2 {
		t.Errorf("normal routes reached Jev %d times, want 2 (one per entry point)", n)
	}
}

// The no-Jev routes return exactly what the normal routes return with Jev disabled.
func TestRouteNoJev_MatchesDisabledRouting(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(2, 0.9)))
	r, _ := newJevRouter(t, stub, config.JevModeActive)
	off, _ := newJevRouter(t, stub, config.JevModeActive, func(c *config.JevRoutingConfig) { c.Enabled = false })
	for _, text := range []string{"Hello", "please refactor the architecture", "think step by step about this"} {
		msgs := userMsg(text)
		got, err1 := r.RouteWithToolsNoJev(msgs, 100, 0, "")
		want, err2 := off.RouteWithTools(msgs, 100, 0, "")
		if err1 != nil || err2 != nil || got.Scenario != want.Scenario || got.Primary.ModelID != want.Primary.ModelID {
			t.Errorf("%q: no-Jev route %+v (%v) differs from disabled route %+v (%v)", text, got, err1, want, err2)
		}
	}
}

// Finding 2: the saturation warning is throttled like the fallback warnings.
func TestJevRouting_SaturationWarningIsThrottled(t *testing.T) {
	entered := make(chan struct{}, 200)
	release := make(chan struct{})
	stub := newJevStub(t, func(_ http.ResponseWriter, req *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-req.Context().Done():
		}
	})
	var once sync.Once
	closeRelease := func() { once.Do(func() { close(release) }) }
	t.Cleanup(closeRelease)
	r, logs := newJevRouter(t, stub, config.JevModeShadow, func(c *config.JevRoutingConfig) { c.TimeoutMs = 5000 })
	for i := 0; i < jevShadowMaxInFlight; i++ {
		route(t, r, userMsg("Hello"), 100, 0)
	}
	for i := 0; i < jevShadowMaxInFlight; i++ {
		<-entered
	}
	for i := 0; i < 50; i++ {
		route(t, r, userMsg("over the bound"), 100, 0)
	}
	closeRelease()
	r.jev.wg.Wait()
	if n := strings.Count(logs.String(), "too many classifications in flight"); n != 1 {
		t.Errorf("saturation warning logged %d times for 50 skipped requests, want exactly 1", n)
	}
}

// Finding 12: a panic in the shadow goroutine is contained and gives its slot back.
type panicTransport struct{}

func (panicTransport) RoundTrip(*http.Request) (*http.Response, error) { panic("boom") }

func TestJevShadow_PanicIsContained(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(1, 0.1)))
	r, logs := newJevRouter(t, stub, config.JevModeShadow)
	r.jev.client = &http.Client{Transport: panicTransport{}}
	route(t, r, userMsg("Hello"), 100, 0)
	r.jev.wg.Wait()
	wantLog(t, logs, "panicked")
	if n := len(r.jev.sem); n != 0 {
		t.Errorf("%d shadow slots still held after a panic", n)
	}
}

// Finding 5: redirects are never followed, so a 307/308 cannot re-send the prompt.
func TestJevRouting_RedirectsAreNotFollowed(t *testing.T) {
	target := newJevStub(t, respondJSON(jevOKBody(2, 0.9)))
	redirector := newJevStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.srv.URL+"/elsewhere")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	r, logs := newJevRouter(t, redirector, config.JevModeActive)
	r.jev.client = nil // exercise the default client built by init()
	res := route(t, r, userMsg("Hello there"), 100, 0)
	if n := target.hits.Load(); n != 0 {
		t.Errorf("the redirect target received %d requests (the prompt was re-sent)", n)
	}
	if redirector.hits.Load() != 1 {
		t.Errorf("redirector hits = %d, want 1", redirector.hits.Load())
	}
	wantLog(t, logs, "keyword scenario stands")
	if res.Scenario != ScenarioDefault && res.Scenario != ScenarioBackground {
		t.Errorf("fail-open route = %s, want the keyword result", res.Scenario)
	}
}

func TestJevRuntime_DefaultClientRefusesRedirects(t *testing.T) {
	var j jevRuntime
	j.init()
	if j.client == nil || j.client.CheckRedirect == nil {
		t.Fatal("default client must be built with a CheckRedirect that refuses redirects")
	}
	target := newJevStub(t, respondJSON("{}"))
	redirector := newJevStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.srv.URL)
		w.WriteHeader(http.StatusPermanentRedirect)
	})
	resp, err := j.client.Post(redirector.srv.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusPermanentRedirect || target.hits.Load() != 0 {
		t.Errorf("status %d, target hits %d: the redirect was followed", resp.StatusCode, target.hits.Load())
	}
}

// Review survivors R3 / N6: only the latest user message is ever sent.
func TestJevRouting_AssistantPrefillIsNeverSent(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(1, 0.1)))
	r, _ := newJevRouter(t, stub, config.JevModeActive)
	msgs := []MessageContent{
		{Role: "user", Content: "hello", PromptText: "hello"},
		{Role: "assistant", Content: "PREFILL", PromptText: "PREFILL"},
	}
	route(t, r, msgs, 100, 0)
	body := string(stub.last.Load().body)
	if !strings.Contains(body, `"state":{"request":"hello"}`) || strings.Contains(body, "PREFILL") {
		t.Errorf("state must be exactly the latest user text: %s", body)
	}
}

func TestJevRouting_EarlierUserTurnIsNeverSentInstead(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(1, 0.1)))
	for _, mode := range []string{config.JevModeShadow, config.JevModeActive} {
		r, _ := newJevRouter(t, stub, mode)
		msgs := []MessageContent{
			{Role: "user", Content: "older authored", PromptText: "older authored"},
			{Role: "assistant", Content: "[Tool Use: Read]"},
			{Role: "user", Content: "TOOL OUTPUT", PromptText: ""}, // tool_result-only turn
		}
		route(t, r, msgs, 100, 0)
		r.jev.wg.Wait()
	}
	if n := stub.hits.Load(); n != 0 {
		t.Errorf("an earlier user turn was sent %d times when the latest had no authored text", n)
	}
}

// Review survivors R19 / T5: pin the documented values with literals.
func TestJevConstantsMatchTheDocs(t *testing.T) {
	if jevShadowMaxInFlight != 16 {
		t.Errorf("jevShadowMaxInFlight = %d; CONFIGURATION.md documents 16", jevShadowMaxInFlight)
	}
	if jevWarnInterval != time.Minute {
		t.Errorf("jevWarnInterval = %v; CONFIGURATION.md documents one minute", jevWarnInterval)
	}
	if maxJevPromptBytes != 64*1024 {
		t.Errorf("maxJevPromptBytes = %d; CONFIGURATION.md documents 64 KB", maxJevPromptBytes)
	}
}
