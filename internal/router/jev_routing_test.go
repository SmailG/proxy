package router

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/routatic/proxy/internal/config"
	"github.com/routatic/proxy/internal/core"
)

// syncBuffer is a log sink safe for the shadow goroutine and the test to share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newJevRouter builds a router whose Jev client talks to a stub server and
// whose log lines land in the returned buffer.
func newJevRouter(t *testing.T, stub *jevStub, mode string, mutate ...func(*config.JevRoutingConfig)) (*ModelRouter, *syncBuffer) {
	t.Helper()
	t.Setenv(jevTestKeyEnv, jevTestKey)
	jcfg := jevTestConfig(stub.srv.URL)
	jcfg.Mode = mode
	for _, m := range mutate {
		m(&jcfg)
	}
	cfg := &config.Config{
		Models: map[string]config.ModelConfig{
			"default":    {Provider: "opencode-go", ModelID: "m-default"},
			"background": {Provider: "opencode-go", ModelID: "m-background"},
			"complex":    {Provider: "opencode-go", ModelID: "m-complex"},
			"think":      {Provider: "opencode-go", ModelID: "m-think"},
			"fast":       {Provider: "opencode-go", ModelID: "m-fast"},
		},
		JevRouting: jcfg,
	}
	r := NewModelRouter(newTestAtomicConfig(cfg))
	logs := &syncBuffer{}
	r.jev.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r.jev.client = stub.srv.Client()
	return r, logs
}

func userMsg(text string) []MessageContent {
	return []MessageContent{{Role: "user", Content: text, PromptText: text}}
}

func route(t *testing.T, r *ModelRouter, msgs []MessageContent, tokens, tools int) RouteResult {
	t.Helper()
	res, err := r.RouteWithTools(msgs, tokens, tools, "")
	if err != nil {
		t.Fatalf("RouteWithTools: %v", err)
	}
	return res
}

func wantLog(t *testing.T, logs *syncBuffer, parts ...string) {
	t.Helper()
	out := logs.String()
	for _, p := range parts {
		if !strings.Contains(out, p) {
			t.Errorf("log lacks %q:\n%s", p, out)
		}
	}
	if strings.Contains(out, jevTestKey) {
		t.Errorf("log contains the API key:\n%s", out)
	}
}

// A negation the keyword matcher gets wrong: "optimize" fires complex.
const negationText = "keep it simple, don't over-optimize anything"

// --- disabled ---------------------------------------------------------------

func TestJevRouting_DisabledIsIdenticalToKeywordRouting(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(2, 0.9)))
	r, logs := newJevRouter(t, stub, config.JevModeActive, func(c *config.JevRoutingConfig) { c.Enabled = false })
	cases := [][]MessageContent{
		userMsg(negationText), userMsg("Think through this"), userMsg("what is Go"),
		userMsg("Hello"), userMsg("Architect a service"), userMsg(""),
	}
	for _, msgs := range cases {
		want := DetectScenarioWithTools(msgs, 100, 0, r.atomic.Get())
		got := route(t, r, msgs, 100, 0)
		if got.Scenario != want.Scenario {
			t.Errorf("%q: scenario %s, keyword routing gives %s", msgs[0].Content, got.Scenario, want.Scenario)
		}
		if !strings.Contains(got.Reason, want.Reason) {
			t.Errorf("%q: reason %q lost keyword reason %q", msgs[0].Content, got.Reason, want.Reason)
		}
	}
	r.jev.wg.Wait()
	if n := stub.hits.Load(); n != 0 {
		t.Errorf("disabled config made %d Jev requests", n)
	}
	if logs.String() != "" {
		t.Errorf("disabled config logged: %s", logs.String())
	}
}

// --- exact checks stay ahead of Jev -----------------------------------------

func TestJevRouting_ExactChecksRunBeforeJev(t *testing.T) {
	image := []MessageContent{{Role: "user", Content: "look", PromptText: "look", HasImage: true}}
	cases := []struct {
		name   string
		msgs   []MessageContent
		tokens int
		tools  int
		want   Scenario
	}{
		{"long context", userMsg("hello"), 500000, 0, ScenarioLongContext},
		{"tool heavy", userMsg("hello"), 100, toolHeavyThreshold, ScenarioComplex},
		{"new image", image, 100, 0, ScenarioVision},
	}
	for _, mode := range []string{config.JevModeShadow, config.JevModeActive} {
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				stub := newJevStub(t, respondJSON(jevOKBody(0, 0.1)))
				r, _ := newJevRouter(t, stub, mode)
				r.atomic.Get().Models["vision"] = config.ModelConfig{Provider: "opencode-go", ModelID: "m-vision", Vision: true}
				r.atomic.Get().Models["long_context"] = config.ModelConfig{Provider: "opencode-go", ModelID: "m-long", ContextThreshold: 100000}
				r.atomic.Get().Fallbacks = map[string][]config.ModelConfig{"vision": {{Provider: "opencode-go", ModelID: "m-vision2"}}}
				got := route(t, r, tc.msgs, tc.tokens, tc.tools)
				r.jev.wg.Wait()
				if got.Scenario != tc.want {
					t.Errorf("scenario = %s, want %s", got.Scenario, tc.want)
				}
				if n := stub.hits.Load(); n != 0 {
					t.Errorf("Jev was called %d times for an exact-check request", n)
				}
			})
		}
	}
}

// --- shadow -----------------------------------------------------------------

func TestJevRouting_ShadowRoutesKeywordScenarioWhenJevDisagrees(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(1, 0.12)))
	r, logs := newJevRouter(t, stub, config.JevModeShadow)
	got := route(t, r, userMsg(negationText), 100, 0)
	r.jev.wg.Wait()
	if got.Scenario != ScenarioComplex {
		t.Fatalf("shadow mode routed %s, want the keyword scenario complex", got.Scenario)
	}
	wantLog(t, logs, "mode=shadow", "keyword_scenario=complex", "jev_level=1",
		"jev_scenario=default", "architecture_prob=0.12", "agree=false", "latency_ms=")
	if n := stub.hits.Load(); n != 1 {
		t.Errorf("Jev requests = %d, want exactly 1", n)
	}
}

func TestJevRouting_ShadowLogsAgreement(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(2, 0.91)))
	r, logs := newJevRouter(t, stub, config.JevModeShadow)
	got := route(t, r, userMsg("Architect a new microservice"), 100, 0)
	r.jev.wg.Wait()
	if got.Scenario != ScenarioComplex {
		t.Fatalf("scenario = %s", got.Scenario)
	}
	wantLog(t, logs, "agree=true", "jev_scenario=complex", "architecture_prob=0.91")
}

// Shadow mode must not delay the request: with a Jev that never answers, the
// route returns while the call is still in flight, and the call then ends at the
// timeout with a logged reason.
func TestJevRouting_ShadowDoesNotDelayTheRequest(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	stub := newJevStub(t, func(_ http.ResponseWriter, req *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-req.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) }) // before srv.Close (LIFO)
	r, logs := newJevRouter(t, stub, config.JevModeShadow, func(c *config.JevRoutingConfig) { c.TimeoutMs = 400 })

	start := time.Now()
	got := route(t, r, userMsg("Hello"), 100, 0)
	elapsed := time.Since(start)
	if got.Scenario != ScenarioDefault {
		t.Errorf("scenario = %s, want default", got.Scenario)
	}
	select {
	case <-entered: // the classification really is running...
	case <-time.After(2 * time.Second):
		t.Fatal("Jev was never called")
	}
	if elapsed > 150*time.Millisecond { // ...yet the 400ms timeout was not waited for
		t.Errorf("route took %v while Jev was blocked; shadow mode must not wait", elapsed)
	}
	r.jev.wg.Wait() // ends by the 400ms timeout, not by release
	wantLog(t, logs, "keyword scenario stands", "deadline exceeded")
}

func TestJevRouting_ShadowBoundsConcurrentClassifications(t *testing.T) {
	entered := make(chan struct{}, jevShadowMaxInFlight+1)
	release := make(chan struct{})
	stub := newJevStub(t, func(_ http.ResponseWriter, req *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-req.Context().Done():
		}
	})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(closeRelease) // before srv.Close (LIFO)
	r, logs := newJevRouter(t, stub, config.JevModeShadow, func(c *config.JevRoutingConfig) { c.TimeoutMs = 5000 })

	for i := 0; i < jevShadowMaxInFlight; i++ {
		route(t, r, userMsg("Hello"), 100, 0)
	}
	for i := 0; i < jevShadowMaxInFlight; i++ {
		<-entered
	}
	route(t, r, userMsg("one too many"), 100, 0) // must be skipped, synchronously
	wantLog(t, logs, "too many classifications in flight")
	closeRelease()
	r.jev.wg.Wait()
	if n := stub.hits.Load(); n != jevShadowMaxInFlight {
		t.Errorf("Jev requests = %d, want %d: the request over the bound must not be sent", n, jevShadowMaxInFlight)
	}
}

// Shadow mode maps a level with the same function active mode uses, so its
// jev_scenario (and agree flag) shows what active mode would do.
func TestJevRouting_ShadowAppliesTheBackgroundPrecondition(t *testing.T) {
	cases := []struct {
		name, text, wantScenario string
	}{
		{"level 0, no tool keyword", "Hello", "jev_scenario=background"},
		{"level 0, tool keyword present", "show me the file, don't edit it", "jev_scenario=default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newJevStub(t, respondJSON(jevOKBody(0, 0.1)))
			r, logs := newJevRouter(t, stub, config.JevModeShadow)
			route(t, r, userMsg(tc.text), 100, 0)
			r.jev.wg.Wait()
			wantLog(t, logs, "jev_level=0", tc.wantScenario)
		})
	}
}

func TestJevRouting_ActiveKeepsTokenCount(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(2, 0.1)))
	r, _ := newJevRouter(t, stub, config.JevModeActive)
	got := r.detectScenario(userMsg("Hello"), 4321, 0, r.atomic.Get())
	if got.Scenario != ScenarioComplex || got.TokenCount != 4321 {
		t.Errorf("got scenario %s with token count %d, want complex with 4321", got.Scenario, got.TokenCount)
	}
}

// Without an injected logger the router logs through slog's default logger.
func TestJevRouting_LogsThroughDefaultLoggerWhenNoneInjected(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(1, 0.1)))
	r, _ := newJevRouter(t, stub, config.JevModeActive)
	r.jev.logger = nil
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	route(t, r, userMsg("Hello"), 100, 0)
	wantLog(t, logs, "jev routing", "mode=active")
}

// The (currently unwired) scenario policy builds its own router messages; Jev
// must work through it too.
func TestJevRouting_ScenarioPolicyCarriesPromptText(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(2, 0.1)))
	r, _ := newJevRouter(t, stub, config.JevModeActive)
	ctx := &EvaluationContext{
		Request: &core.NormalizedRequest{Messages: []core.NormalizedMessage{
			{Role: "user", Blocks: []core.NormalizedContentBlock{{Type: "text", Text: "Hello"}}},
		}},
		TokenCount: 100,
	}
	_, decision, err := NewScenarioPolicy(r).Evaluate(ctx)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.ModelID != "m-complex" || stub.hits.Load() != 1 {
		t.Errorf("decision model %q after %d Jev requests, want m-complex after 1", decision.ModelID, stub.hits.Load())
	}
}
