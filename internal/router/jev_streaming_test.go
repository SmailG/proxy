package router

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/routatic/proxy/internal/config"
)

func routeStream(t *testing.T, r *ModelRouter, msgs []MessageContent, tokens int) RouteResult {
	t.Helper()
	res, err := r.RouteForStreaming(msgs, tokens, "")
	if err != nil {
		t.Fatalf("RouteForStreaming: %v", err)
	}
	return res
}

// Shadow mode samples streaming requests too, logs the same line, and leaves the
// route (including the complex/think -> fast downgrade) exactly as it was.
func TestJevStreamingShadow_LogsTheSameLineAndKeepsTheRoute(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(1, 0.12)))
	r, logs := newJevRouter(t, stub, config.JevModeShadow)
	got := routeStream(t, r, userMsg(negationText), 100)
	r.jev.wg.Wait()
	if got.Scenario != ScenarioFast {
		t.Fatalf("scenario = %s, want fast (the complex/think downgrade)", got.Scenario)
	}
	// keyword_scenario is what the keyword matcher decides for the text, the
	// same comparison the non-streaming shadow line makes.
	wantLog(t, logs, "msg=\"jev routing\"", "mode=shadow", "keyword_scenario=complex", "jev_level=1",
		"jev_scenario=default", "architecture_prob=0.12", "agree=false", "latency_ms=")
	if n := stub.hits.Load(); n != 1 {
		t.Errorf("Jev requests = %d, want exactly 1", n)
	}
}

func TestJevStreamingShadow_RouteIsIdenticalToTheDisabledRoute(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(2, 0.9)))
	shadow, _ := newJevRouter(t, stub, config.JevModeShadow)
	off, _ := newJevRouter(t, stub, config.JevModeShadow, func(c *config.JevRoutingConfig) { c.Enabled = false })
	for _, text := range []string{negationText, "Think through this", "Hello", "Architect a service"} {
		a := routeStream(t, shadow, userMsg(text), 100)
		b := routeStream(t, off, userMsg(text), 100)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%q: shadow route %+v differs from disabled route %+v", text, a, b)
		}
	}
	shadow.jev.wg.Wait()
}

// Streaming returns while Jev is still thinking.
func TestJevStreamingShadow_DoesNotDelayTheRequest(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	stub := newJevStub(t, func(_ http.ResponseWriter, req *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-req.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	r, logs := newJevRouter(t, stub, config.JevModeShadow, func(c *config.JevRoutingConfig) { c.TimeoutMs = 400 })

	start := time.Now()
	got := routeStream(t, r, userMsg("Hello"), 100)
	elapsed := time.Since(start)
	if got.Scenario != ScenarioFast {
		t.Errorf("scenario = %s, want fast", got.Scenario)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Jev was never called from the streaming path")
	}
	if elapsed > 150*time.Millisecond {
		t.Errorf("streaming route took %v while Jev was blocked; it must not wait", elapsed)
	}
	r.jev.wg.Wait()
	wantLog(t, logs, "keyword scenario stands", "deadline exceeded")
}

func TestJevStreamingShadow_DisabledConfigDoesNothing(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(2, 0.9)))
	r, logs := newJevRouter(t, stub, config.JevModeShadow, func(c *config.JevRoutingConfig) { c.Enabled = false })
	routeStream(t, r, userMsg(negationText), 100)
	r.jev.wg.Wait()
	if n := stub.hits.Load(); n != 0 {
		t.Errorf("disabled config made %d Jev requests on the streaming path", n)
	}
	if logs.String() != "" {
		t.Errorf("disabled config logged: %s", logs.String())
	}
}

// Active mode: the streaming path still never calls Jev (the non-streaming path
// does; the streaming one is deliberately unchanged -- it routes to fast).
func TestJevStreamingActive_StillMakesNoJevCall(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(0, 0.1)))
	r, logs := newJevRouter(t, stub, config.JevModeActive)
	got := routeStream(t, r, userMsg(negationText), 100)
	r.jev.wg.Wait()
	if got.Scenario != ScenarioFast {
		t.Errorf("scenario = %s, want fast", got.Scenario)
	}
	if n := stub.hits.Load(); n != 0 {
		t.Errorf("active-mode streaming called Jev %d times", n)
	}
	if logs.String() != "" {
		t.Errorf("active-mode streaming logged: %s", logs.String())
	}
}

// The exact checks and the no-authored-text skip apply to streaming as they do
// to the non-streaming shadow path.
func TestJevStreamingShadow_SkipsWhatJevNeverSees(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(1, 0.1)))
	r, logs := newJevRouter(t, stub, config.JevModeShadow)
	longCtx := routeStream(t, r, userMsg("Hello"), 1_000_000)
	if longCtx.Scenario == ScenarioFast {
		t.Fatalf("long-context request routed to fast: %+v", longCtx)
	}
	routeStream(t, r, []MessageContent{{Role: "user", Content: "tool output only"}}, 100)
	r.jev.wg.Wait()
	if n := stub.hits.Load(); n != 0 {
		t.Errorf("Jev was called %d times for a long-context / no-text streaming request", n)
	}
	wantLog(t, logs, "no authored text")
}

// The streaming path shares the 16-slot bound with the non-streaming one.
func TestJevStreamingShadow_SharesTheInFlightBound(t *testing.T) {
	entered := make(chan struct{}, jevShadowMaxInFlight+1)
	release := make(chan struct{})
	stub := newJevStub(t, func(_ http.ResponseWriter, req *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-req.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	r, logs := newJevRouter(t, stub, config.JevModeShadow, func(c *config.JevRoutingConfig) { c.TimeoutMs = 5000 })
	for i := 0; i < jevShadowMaxInFlight; i++ {
		routeStream(t, r, userMsg("Hello"), 100)
	}
	for i := 0; i < jevShadowMaxInFlight; i++ {
		<-entered
	}
	routeStream(t, r, userMsg("one too many"), 100)
	if !strings.Contains(logs.String(), "too many classifications in flight") {
		t.Errorf("17th streaming request was not skipped:\n%s", logs.String())
	}
}
