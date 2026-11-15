package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/routatic/proxy/internal/config"
)

// Questions put to the TypeSafe System One model (Jev). They are all defined
// here so the full text the remote model sees is reviewable in one place. The
// only state sent with them is {"request": <latest user text, cut to
// jevRequestMaxChars>}: no system prompt, history, tool results or file contents.
//
// Question ids (jevActionID, jevArchitectureID) are for code only; the API does
// not send them to the model, so each question and level carries its whole
// meaning in its own text. Score levels are judged independently and the model
// never sees their numbers (docs.typesafe.ai/primitives/score), so each level
// describes a concrete situation that stands on its own.
const (
	jevActionID       = "action"
	jevArchitectureID = "architecture"

	jevActionInstructions = "What kind of work does `request` ask a coding assistant to do?"

	// Noul: HIGH means YES (docs.typesafe.ai/primitives/noul). Log line only.
	jevArchitectureInstructions = "Does `request` ask for an architecture-level or multi-component change?"
)

// jevActionLevels are the Score levels; the index is the level number and each
// one maps to a routing action in jevScenarioForLevel (jev_routing.go).
var jevActionLevels = []string{
	// 0 -> background/fast
	"Trivial or read-only: look something up, explain code or a concept, list files, show a file, check a status, or rename a variable",
	// 1 -> default
	"Ordinary coding work: implement a single function, fix a specific bug, write or update a test, or make a small contained edit",
	// 2 -> complex
	"Needs design or deep reasoning: choose an architecture, a cross-cutting refactor across many files, hard debugging of an unclear failure, or weigh trade-offs between approaches",
}

const (
	// jevRequestMaxChars is our ceiling, not the API's: Jev takes 64k tokens per
	// request and 32k per question, and a whole call — instructions, level
	// criteria and the text — measured 432 input tokens, ~0.7% of that budget.
	// Cutting the text bounds what one request can carry off the machine and what
	// a call costs. Raising it needs no API change, only a re-measurement of the
	// accuracy, latency and token figures quoted in CONFIGURATION.md.
	jevRequestMaxChars  = 600
	jevResponseMaxBytes = 64 << 10
	jevEndpointPath     = "/v1/systemone"
	jevTopLevel         = 2 // highest level number in jevActionLevels
	jevLevels           = jevTopLevel + 1

	// jevProbabilitySumTolerance accepts the rounding the API applies to each
	// probability: three levels reported to two decimals can sum to 0.99.
	jevProbabilitySumTolerance = 0.02
)

// JevResult is one classification: the routing level (0..2), the level with the
// most probability behind it, how sure the model was, the Noul probability that
// the request is architecture-level (log line only), what the call cost and how
// long it took.
type JevResult struct {
	// Level is the score rounded to the nearest level, the decision routing acts
	// on. Argmax is the single level holding the most probability. They differ
	// only when probability is split, which is exactly what Confidence reports.
	// Routing keeps the rounded score because on the labelled set it was right
	// 27/30 and 26/30 over two live runs against argmax's 24/30 both times: on
	// every split answer argmax picked the minority peak. Argmax is therefore
	// evidence for the log line, not a better decision.
	Level            int
	Argmax           int
	Score            float64
	Confidence       float64
	Probabilities    [jevLevels]float64
	ArchitectureProb float64
	InputTokens      int
	Model            string
	Latency          time.Duration
}

type jevQuestion struct {
	Type         string   `json:"type"`
	Instructions string   `json:"instructions"`
	Criteria     []string `json:"criteria,omitempty"`
}

type jevRequestBody struct {
	Model     string                 `json:"model"`
	State     jevState               `json:"state"`
	Questions map[string]jevQuestion `json:"questions"`
}

type jevState struct {
	Request string `json:"request"`
}

// jevProbabilities is the level -> probability map of a Score answer, keyed by
// level number as a string ("0", "1", "2").
//
// It decodes through pointers on purpose. JSON null lands in a float64 as a
// silent zero, so {"0":1,"1":null,"2":null} would otherwise be indistinguishable
// from a real distribution: it has three levels, every value is in range, and it
// sums to 1. Rejecting null here keeps a malformed reply from steering routing.
type jevProbabilities map[string]float64

func (p *jevProbabilities) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		// Deliberately not wrapped: a decode error can quote the response body,
		// and Classify's contract is that error text never carries one.
		return errors.New("response probabilities are not a level-keyed object")
	}
	out := make(jevProbabilities, len(raw))
	for level, value := range raw {
		var v *float64
		if err := json.Unmarshal(value, &v); err != nil {
			return fmt.Errorf("probability for level %s is not a number", level)
		}
		if v == nil {
			return fmt.Errorf("probability for level %s is null", level)
		}
		out[level] = *v
	}
	*p = out
	return nil
}

type jevAnswer struct {
	Type          string           `json:"type"`
	Score         *float64         `json:"score"`
	Confidence    *float64         `json:"confidence"`
	Probabilities jevProbabilities `json:"probabilities"`
	Noul          *float64         `json:"noul"`
}

type jevResponseBody struct {
	// Model is the versioned id that answered, which is how a pinned id is
	// confirmed and an alias drift is noticed.
	Model   string               `json:"model"`
	Answers map[string]jevAnswer `json:"answers"`
	Usage   *jevUsage            `json:"usage"`
}

type jevUsage struct {
	InputTokens int `json:"input_tokens"`
}

// jevClassifier makes one HTTP POST per Classify call. It holds no secret: the
// API key is read from the environment on every call, so hot-reloaded config
// and rotated keys take effect without a restart.
type jevClassifier struct {
	client *http.Client
	cfg    config.JevRoutingConfig
}

func newJevClassifier(client *http.Client, cfg config.JevRoutingConfig) *jevClassifier {
	return &jevClassifier{client: client, cfg: cfg}
}

// Classify asks Jev about text. ctx carries the deadline. Every error means
// "fall back to the keyword result"; error text never contains the API key or a
// response body.
func (c *jevClassifier) Classify(ctx context.Context, text string) (JevResult, error) {
	if strings.TrimSpace(text) == "" {
		return JevResult{}, errors.New("no request text to classify")
	}
	apiKey := os.Getenv(c.cfg.APIKeyEnv)
	if apiKey == "" {
		return JevResult{}, fmt.Errorf("api key environment variable %s is empty", c.cfg.APIKeyEnv)
	}
	start := time.Now()
	resp, err := c.post(ctx, apiKey, truncateChars(text, jevRequestMaxChars))
	if err != nil {
		return JevResult{}, err
	}
	result, err := parseJevResponse(resp)
	result.Latency = time.Since(start)
	return result, err
}

func (c *jevClassifier) post(ctx context.Context, apiKey, text string) (jevResponseBody, error) {
	var out jevResponseBody
	payload, err := json.Marshal(jevRequestBody{
		Model: c.cfg.Model,
		State: jevState{Request: text},
		Questions: map[string]jevQuestion{
			jevActionID:       {Type: "score", Instructions: jevActionInstructions, Criteria: jevActionLevels},
			jevArchitectureID: {Type: "noul", Instructions: jevArchitectureInstructions},
		},
	})
	if err != nil {
		return out, errors.New("encode request")
	}
	url := strings.TrimRight(c.cfg.BaseURL, "/") + jevEndpointPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return out, errors.New("build request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := c.client.Do(req)
	if err != nil {
		// Report only the class of failure: err text can carry the URL.
		return out, fmt.Errorf("request failed: %s", classifyTransportError(ctx))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// The body is not read: 422 names the offending field there, but the same
		// body can echo the credential, and jevStatusClass is enough to act on.
		return out, fmt.Errorf("%s (http %d)", jevStatusClass(resp.StatusCode), resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, jevResponseMaxBytes)).Decode(&out); err != nil {
		return out, errors.New("malformed response body")
	}
	return out, nil
}

func classifyTransportError(ctx context.Context) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "deadline exceeded"
	case errors.Is(ctx.Err(), context.Canceled):
		return "canceled"
	default:
		return "transport error"
	}
}

// jevStatusClass names the failure a status represents, because the fix differs
// and the number alone does not say which applies: 401 is our config, 422 is our
// question, 429 is our traffic, 529 is their capacity. These are the codes
// docs.typesafe.ai/api/errors defines. The text doubles as the warning throttle's
// key, so it must stay finite and carry nothing per-request (URL, body,
// credential); the code rides along beside it. No retry, deliberately: inside
// active mode's timeout_ms budget a backoff only turns a fast fail-open slow.
func jevStatusClass(status int) string {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "api key rejected"
	case http.StatusNotFound:
		return "endpoint not found"
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return "request rejected"
	case http.StatusTooManyRequests:
		return "rate limited"
	case 529: // Overloaded; net/http has no constant for it.
		return "upstream overloaded"
	default:
		if status >= 500 {
			return "upstream error"
		}
		return "unexpected status"
	}
}

// parseJevResponse turns the answers into a JevResult. Anything missing, of the
// wrong type, non-finite or out of range is an error: a half-trusted answer
// must not steer routing.
func parseJevResponse(body jevResponseBody) (JevResult, error) {
	action := body.Answers[jevActionID] // an absent answer has type "" and fails below
	if action.Type != "score" || action.Score == nil {
		return JevResult{}, errors.New("response missing the score answer")
	}
	if action.Confidence == nil {
		return JevResult{}, errors.New("response missing the score confidence")
	}
	probs, err := parseLevelProbabilities(action.Probabilities)
	if err != nil {
		return JevResult{}, err
	}
	arch := body.Answers[jevArchitectureID]
	if arch.Type != "noul" || arch.Noul == nil {
		return JevResult{}, errors.New("response missing the noul answer")
	}
	score, confidence, noul := *action.Score, *action.Confidence, *arch.Noul
	if !inRange(score, 0, jevTopLevel) || !inRange(confidence, 0, 1) || !inRange(noul, 0, 1) {
		return JevResult{}, errors.New("answer value out of range")
	}
	// The score is a probability-weighted mean of level numbers, so it can fall
	// between levels. Rounding to the nearest level is what the Score docs
	// recommend when code needs one outcome; a 50/50 split between levels 0 and
	// 2 rounds to level 1, the default, which is the safe hedge. Argmax is the
	// level the probability actually peaks on, carried beside the decision so the
	// two can be compared, and Confidence is what separates a clear answer from
	// that split.
	res := JevResult{
		Level:            int(math.Round(score)),
		Argmax:           argmaxLevel(probs),
		Score:            score,
		Confidence:       confidence,
		Probabilities:    probs,
		ArchitectureProb: noul,
		Model:            body.Model,
	}
	if body.Usage != nil {
		res.InputTokens = body.Usage.InputTokens
	}
	return res, nil
}

// parseLevelProbabilities reads the per-level distribution of a Score answer.
// Every level must be present and in [0, 1] and together they must add to 1: on
// a partial distribution neither the argmax nor a confidence floor would mean
// what the docs promise.
func parseLevelProbabilities(raw map[string]float64) ([jevLevels]float64, error) {
	var out [jevLevels]float64
	if len(raw) != jevLevels {
		return out, fmt.Errorf("response probabilities have %d levels, want %d", len(raw), jevLevels)
	}
	sum := 0.0
	for level := range out {
		v, ok := raw[strconv.Itoa(level)]
		if !ok {
			return out, fmt.Errorf("response probabilities are missing level %d", level)
		}
		if !inRange(v, 0, 1) {
			return out, fmt.Errorf("probability for level %d out of range", level)
		}
		out[level] = v
		sum += v
	}
	if math.Abs(sum-1) > jevProbabilitySumTolerance {
		return out, errors.New("response probabilities do not sum to 1")
	}
	return out, nil
}

// argmaxLevel returns the level holding the most probability. A tie keeps the
// lower level, so the result never depends on map iteration order.
func argmaxLevel(probs [jevLevels]float64) int {
	best := 0
	for level := 1; level < len(probs); level++ {
		if probs[level] > probs[best] {
			best = level
		}
	}
	return best
}

// inRange is false for NaN, whose comparisons are all false.
func inRange(v, lo, hi float64) bool {
	return v >= lo && v <= hi
}

// truncateChars cuts s to at most max characters (runes), never splitting a
// multi-byte character.
func truncateChars(s string, max int) string {
	if len(s) <= max { // fewer bytes than max means fewer runes than max
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
