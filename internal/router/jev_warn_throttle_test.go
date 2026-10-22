package router

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/routatic/proxy/internal/config"
)

// fakeClock is an injected clock: tests move time by hand and never sleep.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)} }

func countLines(logs *syncBuffer, part string) int {
	return strings.Count(logs.String(), part)
}

func failingRouter(t *testing.T, status int) (*ModelRouter, *syncBuffer, *fakeClock) {
	t.Helper()
	stub := newJevStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"detail":"` + jevTestKey + `"}`)) // hostile body echoing the key
	})
	r, logs := newJevRouter(t, stub, config.JevModeActive)
	clk := newClock()
	r.jev.warn.now = clk.now
	return r, logs, clk
}

func TestJevWarnThrottle_BurstLogsOnce(t *testing.T) {
	r, logs, _ := failingRouter(t, 503)
	for i := 0; i < 25; i++ {
		route(t, r, userMsg(negationText), 100, 0)
	}
	if n := countLines(logs, "http status 503"); n != 1 {
		t.Errorf("burst of 25 failures logged %d warnings, want 1:\n%s", n, logs.String())
	}
	wantLog(t, logs, "level=WARN", "keyword scenario stands") // also asserts the key is absent
}

func TestJevWarnThrottle_LogsAgainAfterTheIntervalWithTheSuppressedCount(t *testing.T) {
	r, logs, clk := failingRouter(t, 503)
	for i := 0; i < 5; i++ { // 1 logged + 4 suppressed
		route(t, r, userMsg(negationText), 100, 0)
	}
	clk.advance(jevWarnInterval - time.Nanosecond)
	route(t, r, userMsg(negationText), 100, 0) // still inside the interval: 5th suppressed
	if n := countLines(logs, "http status 503"); n != 1 {
		t.Fatalf("logged %d times just inside the interval, want 1", n)
	}
	clk.advance(time.Nanosecond) // exactly the interval
	route(t, r, userMsg(negationText), 100, 0)
	if n := countLines(logs, "http status 503"); n != 2 {
		t.Fatalf("logged %d times at the interval, want 2:\n%s", n, logs.String())
	}
	wantLog(t, logs, "suppressed=5")
	if strings.Count(logs.String(), "suppressed=") != 1 {
		t.Errorf("the first occurrence must not carry a suppressed count:\n%s", logs.String())
	}
	// The count restarts after a log line, and the window restarts too.
	route(t, r, userMsg(negationText), 100, 0)
	clk.advance(jevWarnInterval)
	route(t, r, userMsg(negationText), 100, 0)
	if !strings.Contains(logs.String(), "suppressed=1") {
		t.Errorf("third line lacks suppressed=1:\n%s", logs.String())
	}
}

func TestJevWarnThrottle_ReasonsAreIndependent(t *testing.T) {
	stub := newJevStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(429) })
	r, logs := newJevRouter(t, stub, config.JevModeActive)
	r.jev.warn.now = newClock().now
	for i := 0; i < 3; i++ {
		route(t, r, userMsg(negationText), 100, 0)
	}
	t.Setenv(jevTestKeyEnv, "") // a second, different reason: the key is gone
	for i := 0; i < 3; i++ {
		route(t, r, userMsg(negationText), 100, 0)
	}
	if n := countLines(logs, "http status 429"); n != 1 {
		t.Errorf("429 logged %d times, want 1", n)
	}
	if n := countLines(logs, "api key environment variable"); n != 1 {
		t.Errorf("missing-key logged %d times, want 1 (independent of the 429 reason):\n%s", n, logs.String())
	}
}

// The shadow path uses the same throttle.
func TestJevWarnThrottle_ShadowBurstLogsOnce(t *testing.T) {
	stub := newJevStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	r, logs := newJevRouter(t, stub, config.JevModeShadow)
	r.jev.warn.now = newClock().now
	for i := 0; i < 8; i++ {
		route(t, r, userMsg("Hello"), 100, 0)
		r.jev.wg.Wait()
	}
	if n := countLines(logs, "http status 500"); n != 1 {
		t.Errorf("shadow burst logged %d warnings, want 1:\n%s", n, logs.String())
	}
}

func TestJevWarnThrottle_ZeroValueUsesTheRealClock(t *testing.T) {
	var th jevWarnThrottle
	if ok, _ := th.allow("x"); !ok {
		t.Fatal("first occurrence must log")
	}
	if ok, _ := th.allow("x"); ok {
		t.Error("second immediate occurrence must be suppressed")
	}
}
