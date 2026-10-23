package router

import (
	"testing"
	"time"

	"github.com/routatic/proxy/internal/config"
)

// A tool loop: the user asks once, then every turn carries only tool output.
func toolLoop(ask string) []MessageContent {
	return []MessageContent{
		{Role: "user", Content: ask, PromptText: ask},
		{Role: "assistant", Content: "reading the file"},
		{Role: "user", Content: "tool output: done"}, // PromptText empty; keyword result is default
	}
}

func TestJevCache_ToolResultTurnReusesDecisionWithoutCallingJev(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(2, 0.9)))
	r, _ := newJevRouter(t, stub, config.JevModeActive)
	ask := "Hello"
	if got := route(t, r, userMsg(ask), 100, 0); got.Scenario != ScenarioComplex {
		t.Fatalf("first turn = %s, want complex", got.Scenario)
	}
	got := route(t, r, toolLoop(ask), 100, 0)
	if got.Scenario != ScenarioComplex {
		t.Errorf("tool-result turn = %s, want the cached complex", got.Scenario)
	}
	if n := stub.hits.Load(); n != 1 {
		t.Errorf("Jev calls = %d, want 1 (tool-result turn must not call it)", n)
	}
}

func TestJevCache_MissFallsBackToKeyword(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(2, 0.9)))
	r, _ := newJevRouter(t, stub, config.JevModeActive)
	// Nothing was classified for this ask: keyword result from the tool output stands.
	got := route(t, r, toolLoop("never classified"), 100, 0)
	if got.Scenario != ScenarioDefault || stub.hits.Load() != 0 {
		t.Errorf("scenario=%s hits=%d, want keyword default and no Jev call", got.Scenario, stub.hits.Load())
	}
}

func TestJevCache_ShadowModeNeverServesFromCache(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(2, 0.9)))
	r, _ := newJevRouter(t, stub, config.JevModeShadow)
	route(t, r, userMsg("Hello"), 100, 0)
	r.jev.wg.Wait()
	if got := route(t, r, toolLoop("Hello"), 100, 0); got.Scenario != ScenarioDefault {
		t.Errorf("shadow tool-result turn = %s, want keyword default", got.Scenario)
	}
}

func TestJevDecisionCache_ExpiryAndBound(t *testing.T) {
	now := time.Unix(1000, 0)
	c := &jevDecisionCache{now: func() time.Time { return now }}
	k := jevKey("a")
	c.put(k, JevResult{Level: 2})
	if _, ok := c.get(k); !ok {
		t.Fatal("fresh entry missing")
	}
	now = now.Add(jevCacheTTL + time.Second)
	if _, ok := c.get(k); ok {
		t.Error("expired entry served")
	}
	for i := 0; i < jevCacheMax+50; i++ {
		now = now.Add(time.Millisecond)
		c.put(jevKey(string(rune('A'+i%50))+time.Duration(i).String()), JevResult{})
	}
	if len(c.m) > jevCacheMax {
		t.Errorf("cache size %d exceeds bound %d", len(c.m), jevCacheMax)
	}
}
