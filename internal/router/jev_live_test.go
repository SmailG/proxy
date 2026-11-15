package router

import (
	"context"
	"math"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/routatic/proxy/internal/config"
)

// TestJevLive_LabelledCases sends testdata/jev_eval_cases.jsonl to the REAL TypeSafe
// API and scores what comes back: the rounded level and the argmax level against
// the human label and against the keyword matcher, the confidence distribution,
// what each candidate min_confidence floor would cost in coverage and buy in
// accuracy, repeat-call stability, latency and input tokens. It is opt-in
// (JEV_LIVE_TEST=1 plus the key in the environment variable named by
// JEV_LIVE_KEY_ENV, default TYPESAFE_AI_API_KEY) and costs well under a cent.
// It reports and only fails when the API is unusable: the labels are one person's
// reading of 30 requests taken from the existing router tests, so the numbers
// are evidence to read, not a gate.
//
//	JEV_LIVE_TEST=1 go test ./internal/router/ -run JevLive -v -count=1
//
// JEV_LIVE_BASE_URL points the same 30 cases at another System One endpoint
// (Alibaba Model Studio's decision model), which is the only way to compare two
// providers against one set of labels.
func TestJevLive_LabelledCases(t *testing.T) {
	if os.Getenv("JEV_LIVE_TEST") != "1" {
		t.Skip("set JEV_LIVE_TEST=1 to call the real TypeSafe API")
	}
	keyEnv := os.Getenv("JEV_LIVE_KEY_ENV")
	if keyEnv == "" {
		keyEnv = "TYPESAFE_AI_API_KEY"
	}
	if os.Getenv(keyEnv) == "" {
		t.Fatalf("%s is empty", keyEnv)
	}
	baseURL := os.Getenv("JEV_LIVE_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.typesafe.ai"
	}
	jcfg := config.JevRoutingConfig{
		Enabled: true, Mode: config.JevModeShadow, Model: os.Getenv("JEV_LIVE_MODEL"),
		TimeoutMs: 5000, APIKeyEnv: keyEnv, BaseURL: baseURL,
	}
	if jcfg.Model == "" {
		jcfg.Model = "jev-1.14.0"
	}
	var rt jevRuntime
	rt.init()
	clf := newJevClassifier(rt.client, jcfg)

	cases := loadEvalCases(t)
	type row struct {
		text                                  string
		label, jev, argmax, jev2, argmax2, kw int
		confidence, arch                      float64
		tokens                                int
		model                                 string
		ms                                    int64
	}
	var rows []row
	failures := 0
	for _, c := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		res, err := clf.Classify(ctx, c.Text)
		res2, err2 := clf.Classify(ctx, c.Text)
		cancel()
		if err != nil || err2 != nil {
			failures++
			t.Logf("API error for %q: %v / %v", c.Text, err, err2)
			continue
		}
		rows = append(rows, row{
			text: c.Text, label: c.Level, jev: res.Level, argmax: res.Argmax,
			jev2: res2.Level, argmax2: res2.Argmax, kw: keywordLevel(c.Text),
			confidence: res.Confidence, arch: res.ArchitectureProb,
			tokens: res.InputTokens, model: res.Model, ms: res.Latency.Milliseconds(),
		})
	}
	if len(rows) == 0 {
		t.Fatalf("no successful call out of %d cases", len(cases))
	}

	var jevOK, argmaxOK, kwOK, stable, argmaxStable, split, splitJevOK, splitArgmaxOK int
	var confRight, confWrong, confidences []float64
	var lat []int64
	tokens := 0
	models := map[string]int{}
	for _, r := range rows {
		jevRight := r.jev == r.label
		argmaxRight := r.argmax == r.label
		if jevRight {
			jevOK++
			confRight = append(confRight, r.confidence)
		} else {
			confWrong = append(confWrong, r.confidence)
		}
		if argmaxRight {
			argmaxOK++
		}
		if r.kw == r.label {
			kwOK++
		}
		if r.jev == r.jev2 {
			stable++
		}
		if r.argmax == r.argmax2 {
			argmaxStable++
		}
		// A split answer is the only case where the two readings can disagree.
		if r.jev != r.argmax {
			split++
			if jevRight {
				splitJevOK++
			}
			if argmaxRight {
				splitArgmaxOK++
			}
		}
		confidences = append(confidences, r.confidence)
		lat = append(lat, r.ms)
		tokens += r.tokens
		models[r.model]++
		mark := " "
		if !jevRight {
			mark = "J" // rounded level disagrees with the label
		}
		if !argmaxRight {
			mark += "A" // argmax disagrees with the label
		}
		if r.kw != r.label {
			mark += "K" // keyword matcher disagrees with the label
		}
		t.Logf("%-4s label=%d round=%d argmax=%d conf=%.2f keyword=%d arch=%.2f tok=%d %4dms  %s",
			mark, r.label, r.jev, r.argmax, r.confidence, r.kw, r.arch, r.tokens, r.ms, truncate(r.text, 60))
	}

	n := len(rows)
	t.Logf("answered by %v over %d cases, %d api errors", models, n, failures)
	t.Logf("exact level: rounded score %d/%d (%.0f%%), argmax %d/%d (%.0f%%), keyword matcher %d/%d (%.0f%%)",
		jevOK, n, pct(jevOK, n), argmaxOK, n, pct(argmaxOK, n), kwOK, n, pct(kwOK, n))
	t.Logf("round != argmax on %d/%d cases; there the rounded score was right %d times and argmax %d times",
		split, n, splitJevOK, splitArgmaxOK)
	t.Logf("repeat-call stability: rounded level %d/%d, argmax %d/%d", stable, n, argmaxStable, n)
	t.Logf("confidence: min=%.2f p50=%.2f max=%.2f; mean when right=%.2f (n=%d), when wrong=%.2f (n=%d)",
		minConfidence(confidences), percentile(confidences, 0.5), maxConfidence(confidences),
		mean(confRight), len(confRight), mean(confWrong), len(confWrong))
	// What each candidate floor would cost in coverage and buy in accuracy.
	for _, floor := range []float64{0.3, 0.4, 0.5, 0.6, 0.7, 0.8} {
		keptRound, keptArgmax, kept := 0, 0, 0
		for _, r := range rows {
			if r.confidence < floor {
				continue
			}
			kept++
			if r.jev == r.label {
				keptRound++
			}
			if r.argmax == r.label {
				keptArgmax++
			}
		}
		if kept == 0 {
			t.Logf("min_confidence=%.1f: abstains on all %d cases", floor, n)
			continue
		}
		t.Logf("min_confidence=%.1f: acts on %d/%d (%.0f%%), rounded score right %d/%d (%.0f%%), argmax right %d/%d (%.0f%%)",
			floor, kept, n, pct(kept, n), keptRound, kept, pct(keptRound, kept), keptArgmax, kept, pct(keptArgmax, kept))
	}
	t.Logf("latency ms: p50=%d p95=%d max=%d", percentileInt(lat, 0.5), percentileInt(lat, 0.95), maxLatency(lat))
	t.Logf("input tokens: %d total, mean %.0f per call", tokens, float64(tokens)/float64(n))
}

func pct(part, whole int) float64 { return 100 * float64(part) / float64(whole) }

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

func minConfidence(v []float64) float64 {
	best := math.Inf(1)
	for _, x := range v {
		if x < best {
			best = x
		}
	}
	return best
}

func maxConfidence(v []float64) float64 {
	best := math.Inf(-1)
	for _, x := range v {
		if x > best {
			best = x
		}
	}
	return best
}

// percentile sorts a copy and takes the nearest-rank value.
func percentile(v []float64, q float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[rank(len(s), q)]
}

func percentileInt(v []int64, q float64) int64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[rank(len(s), q)]
}

func rank(n int, q float64) int {
	i := int(math.Ceil(q*float64(n))) - 1
	if i < 0 {
		i = 0
	}
	if i >= n {
		i = n - 1
	}
	return i
}

func maxLatency(v []int64) int64 {
	best := int64(math.MinInt64)
	for _, x := range v {
		if x > best {
			best = x
		}
	}
	return best
}

// keywordLevel maps the keyword matcher's scenario to the same 0..2 scale as the labels.
func keywordLevel(text string) int {
	switch detectKeywordScenario(userMsg(text), 50).Scenario {
	case ScenarioComplex, ScenarioThink:
		return 2
	case ScenarioBackground:
		return 0
	default:
		return 1
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
