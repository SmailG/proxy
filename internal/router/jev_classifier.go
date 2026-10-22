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
	jevRequestMaxChars  = 600
	jevResponseMaxBytes = 64 << 10
	jevEndpointPath     = "/v1/systemone"
	jevTopLevel         = 2 // highest level number in jevActionLevels
)

// JevResult is one classification: the routing level (0..2), the Noul
// probability that the request is architecture-level (log line only) and how
// long the call took.
type JevResult struct {
	Level            int
	Score            float64
	ArchitectureProb float64
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

type jevAnswer struct {
	Type  string   `json:"type"`
	Score *float64 `json:"score"`
	Noul  *float64 `json:"noul"`
}

type jevResponseBody struct {
	Answers map[string]jevAnswer `json:"answers"`
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
		return out, fmt.Errorf("http status %d", resp.StatusCode)
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

// parseJevResponse turns the answers into a JevResult. Anything missing, of the
// wrong type, non-finite or out of range is an error: a half-trusted answer
// must not steer routing.
func parseJevResponse(body jevResponseBody) (JevResult, error) {
	action, ok := body.Answers[jevActionID]
	if !ok || action.Type != "score" || action.Score == nil {
		return JevResult{}, errors.New("response missing the score answer")
	}
	arch, ok := body.Answers[jevArchitectureID]
	if !ok || arch.Type != "noul" || arch.Noul == nil {
		return JevResult{}, errors.New("response missing the noul answer")
	}
	score, noul := *action.Score, *arch.Noul
	if !inRange(score, 0, jevTopLevel) || !inRange(noul, 0, 1) {
		return JevResult{}, errors.New("answer value out of range")
	}
	// The score is a probability-weighted mean of level numbers, so it can fall
	// between levels. Rounding to the nearest level is what the Score docs
	// recommend when code needs one outcome; a 50/50 split between levels 0 and
	// 2 rounds to level 1, the default, which is the safe hedge.
	return JevResult{Level: int(math.Round(score)), Score: score, ArchitectureProb: noul}, nil
}

func inRange(v, lo, hi float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= lo && v <= hi
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
