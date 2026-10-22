package router

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/routatic/proxy/internal/config"
)

// jevShadowMaxInFlight bounds the goroutines shadow mode can have outstanding.
// Each one lives at most timeout_ms, so this only bites when Jev is slow and
// traffic is heavy; excess requests are simply not sampled.
const jevShadowMaxInFlight = 16

// jevRuntime is the per-router state of the classifier: one shared HTTP client
// (connection reuse), the shadow-mode concurrency bound, and the logger.
// The zero value is usable; init fills in anything a test did not inject.
type jevRuntime struct {
	once   sync.Once
	client *http.Client
	sem    chan struct{}
	wg     sync.WaitGroup
	logger *slog.Logger
	warn   jevWarnThrottle
}

func (j *jevRuntime) init() {
	j.once.Do(func() {
		if j.client == nil {
			j.client = &http.Client{} // deadlines come from the per-call context
		}
		if j.sem == nil {
			j.sem = make(chan struct{}, jevShadowMaxInFlight)
		}
	})
}

func (j *jevRuntime) log() *slog.Logger {
	if j.logger != nil {
		return j.logger
	}
	return slog.Default()
}

// detectScenario is the router's scenario decision. With jev_routing disabled it
// is exactly DetectScenarioWithTools. When enabled, the exact checks (token
// count, new image, tool count) still run first and win; only a request they
// leave undecided reaches the keyword matcher and, beside or instead of it, Jev.
func (r *ModelRouter) detectScenario(messages []MessageContent, tokenCount, toolCount int, cfg *config.Config) ScenarioResult {
	if !cfg.JevRouting.Enabled {
		return DetectScenarioWithTools(messages, tokenCount, toolCount, cfg)
	}
	if exact, ok := detectExactScenario(messages, tokenCount, toolCount, cfg); ok {
		return exact
	}
	keyword := detectKeywordScenario(messages, tokenCount)
	text := latestPromptText(messages)
	if strings.TrimSpace(text) == "" {
		r.jev.log().Debug("jev routing skipped: latest user message has no authored text", "keyword_scenario", keyword.Scenario)
		return keyword
	}
	r.jev.init()
	if cfg.JevRouting.Mode == config.JevModeActive {
		return r.jevActive(cfg.JevRouting, messages, keyword, text)
	}
	r.jevShadow(cfg.JevRouting, messages, keyword, text)
	return keyword
}

// latestPromptText is the authored text of the latest user message, with the
// harness's <system-reminder> blocks removed (stripHarnessBlocks). It never
// falls back to MessageContent.Content, which carries tool output.
func latestPromptText(messages []MessageContent) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return stripHarnessBlocks(messages[i].PromptText)
		}
	}
	return ""
}

// jevStreamingShadow is the shadow-mode Jev call for the streaming path, which
// routes to fast and otherwise never reaches detectScenario. It only logs: the
// streaming route (including the complex/think -> fast downgrade) is untouched.
// Active mode makes no Jev call here, as before. keyword_scenario in the log is
// the scenario the keyword matcher picks for the text, the same comparison the
// non-streaming shadow line makes, not the fast scenario the stream is sent to.
// The tool count is not known on this path, so the tool-heavy exact check is not
// applied; the token-count and image checks are.
func (r *ModelRouter) jevStreamingShadow(messages []MessageContent, tokenCount int, cfg *config.Config) {
	if !cfg.JevRouting.Enabled || cfg.JevRouting.Mode != config.JevModeShadow {
		return
	}
	if _, decided := detectExactScenario(messages, tokenCount, 0, cfg); decided {
		return
	}
	keyword := detectKeywordScenario(messages, tokenCount)
	text := latestPromptText(messages)
	if strings.TrimSpace(text) == "" {
		r.jev.log().Debug("jev routing skipped: latest user message has no authored text", "keyword_scenario", keyword.Scenario)
		return
	}
	r.jev.init()
	r.jevShadow(cfg.JevRouting, messages, keyword, text)
}

// jevActive lets Jev decide. It blocks the request for at most timeout_ms; any
// failure returns the keyword result unchanged.
func (r *ModelRouter) jevActive(jcfg config.JevRoutingConfig, messages []MessageContent, keyword ScenarioResult, text string) ScenarioResult {
	res, err := r.classify(jcfg, text)
	if err != nil {
		r.logJevFallback(jcfg.Mode, keyword, err)
		return keyword
	}
	scenario := jevScenarioForLevel(res.Level, hasToolBlocker(messages))
	r.logJevResult(jcfg.Mode, keyword, scenario, res)
	return ScenarioResult{
		Scenario:   scenario,
		TokenCount: keyword.TokenCount,
		Reason:     fmt.Sprintf("semantic classifier level %d (%s)", res.Level, jevLevelLabels[res.Level]),
	}
}

// jevShadow classifies in a goroutine and only logs. The request never waits:
// the routed scenario is the keyword one and this returns immediately, so
// shadow mode adds no latency whatever Jev does. The goroutine is bounded by
// timeout_ms and by jevShadowMaxInFlight.
func (r *ModelRouter) jevShadow(jcfg config.JevRoutingConfig, messages []MessageContent, keyword ScenarioResult, text string) {
	select {
	case r.jev.sem <- struct{}{}:
	default:
		r.jev.log().Warn("jev shadow skipped: too many classifications in flight",
			"limit", jevShadowMaxInFlight, "keyword_scenario", keyword.Scenario)
		return
	}
	blocked := hasToolBlocker(messages) // evaluated now: messages belongs to the caller
	r.jev.wg.Add(1)
	go func() {
		defer r.jev.wg.Done()
		defer func() { <-r.jev.sem }()
		res, err := r.classify(jcfg, text)
		if err != nil {
			r.logJevFallback(jcfg.Mode, keyword, err)
			return
		}
		r.logJevResult(jcfg.Mode, keyword, jevScenarioForLevel(res.Level, blocked), res)
	}()
}

func (r *ModelRouter) classify(jcfg config.JevRoutingConfig, text string) (JevResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(jcfg.TimeoutMs)*time.Millisecond)
	defer cancel()
	return newJevClassifier(r.jev.client, jcfg).Classify(ctx, text)
}

// jevLevelLabels describe each level in a Reason string. They name no model.
var jevLevelLabels = [...]string{
	"trivial or read-only request",
	"ordinary coding work",
	"needs design or deep reasoning",
}

// jevScenarioForLevel maps a Jev level to a routing scenario. Level 0 goes to
// background only when the existing background precondition holds (no tool
// keywords anywhere in the messages); otherwise it stays on default.
func jevScenarioForLevel(level int, toolBlocked bool) Scenario {
	switch level {
	case 2:
		return ScenarioComplex
	case 0:
		if toolBlocked {
			return ScenarioDefault
		}
		return ScenarioBackground
	default:
		return ScenarioDefault
	}
}

func (r *ModelRouter) logJevResult(mode string, keyword ScenarioResult, jev Scenario, res JevResult) {
	r.jev.log().Info("jev routing",
		"mode", mode,
		"keyword_scenario", keyword.Scenario,
		"jev_level", res.Level,
		"jev_scenario", jev,
		"architecture_prob", res.ArchitectureProb,
		"agree", keyword.Scenario == jev,
		"latency_ms", res.Latency.Milliseconds(),
	)
}

// logJevFallback warns that Jev failed and the keyword scenario stands. It is
// throttled per reason (jevWarnThrottle) so an outage is one line a minute, not
// one per request.
func (r *ModelRouter) logJevFallback(mode string, keyword ScenarioResult, err error) {
	reason := err.Error()
	ok, suppressed := r.jev.warn.allow(reason)
	if !ok {
		return
	}
	attrs := []any{"mode", mode, "keyword_scenario", keyword.Scenario, "reason", reason}
	if suppressed > 0 {
		attrs = append(attrs, "suppressed", suppressed)
	}
	r.jev.log().Warn("jev routing failed, keyword scenario stands", attrs...)
}
