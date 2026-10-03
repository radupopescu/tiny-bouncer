package jev

// The Jev backend proper (architecture §5bis): battery + verdict mapping
// wired onto the System One transport from the T05 client, registered in the
// backend registry. One request per command, bounded fan-out, fail-safe to
// ask for entries it cannot judge.
//
// Certainty discipline (architecture §5.3) is inherent in the route: a deny
// fires only above its gates, nothing is ever auto-upgraded from deny or ask
// to allow, and thresholds hold only where a gate fired.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"

	"wiseyolo/internal/backend"
	"wiseyolo/internal/core"
)

// ModelListPath is the endpoint path used by HealthCheck.
const ModelListPath = "/v1/models"

func init() {
	backend.Register("jev", factory)
}

// DefaultConcurrency is the bounded fan-out width (architecture §5, step 3).
const DefaultConcurrency = 5

// concurrencyEnv overrides the fan-out width.
const concurrencyEnv = "WISE_YOLO_CONCURRENCY"

// factory builds the backend from backend.Config.Values falling back to the
// process environment (the same precedence everywhere). A missing API key is
// a typed ConfigError so `check` exits 1 with a clear message; an invalid
// thresholds or concurrency override is one likewise.
func factory(cfg backend.Config) (backend.Backend, error) {
	lookup := func(key string) string {
		if cfg.Values != nil {
			if v := cfg.Values[key]; v != "" {
				return v
			}
		}
		return os.Getenv(key)
	}
	client, err := NewWithLookup(lookup)
	if err != nil {
		return nil, err
	}
	t, err := LoadThresholds(lookup)
	if err != nil {
		return nil, err
	}
	b := &jevBackend{client: client, thresholds: t, concurrency: DefaultConcurrency}
	if v := lookup(concurrencyEnv); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, &ConfigError{Message: fmt.Sprintf("jev: invalid %s %q", concurrencyEnv, v)}
		}
		b.concurrency = n
	}
	return b, nil
}

// jevBackend implements backend.Backend for Jev. Safe for concurrent use.
type jevBackend struct {
	client     *Client
	thresholds Thresholds
	// concurrency bounds the fan-out width, default 5 (WISE_YOLO_CONCURRENCY).
	concurrency int

	// resolvedModel is the resolved model name from the most recent
	// successful response (e.g. jev-1.13.0), not the alias sent. It feeds
	// Info() so meta.backend_model reports the resolved model.
	mu            sync.Mutex
	resolvedModel string
	// totalAttempts accumulates Send attempt counts for the meta pipeline.
	totalAttempts int
}

func (b *jevBackend) Name() string { return "jev" }

// Info returns the identity facts recorded in meta. After any response has
// resolved, Model is the response's model field; before then, the configured
// model (WISE_YOLO_JEV_MODEL, default jev-latest).
func (b *jevBackend) Info() backend.Info {
	model := b.client.cfg.Model
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.resolvedModel != "" {
		model = b.resolvedModel
	}
	return backend.Info{
		Name:              "jev",
		Model:             model,
		PolicyVersion:     PolicyVersion,
		ThresholdsVersion: ThresholdsVersion,
	}
}

// HealthCheck verifies the credentials and reachability by asking the
// endpoint for its model list (architecture §5bis; feeds T07 doctor):
// GET /v1/models with auth. 200 → nil; anything else is an error naming the
// status and a hint.
func (b *jevBackend) HealthCheck(ctx context.Context) error {
	url := b.client.cfg.BaseURL + ModelListPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("jev: build model-list request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+b.client.cfg.APIKey)
	res, err := b.client.hc.Do(req)
	if err != nil {
		return fmt.Errorf("jev: endpoint unreachable: %v", err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode == http.StatusOK {
		return nil
	}
	if res.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("jev: health check failed (401): check WISE_YOLO_JEV_API_KEY or TYPESAFE_API_KEY")
	}
	return fmt.Errorf("jev: health check failed (status %d): endpoint returned an error", res.StatusCode)
}

// classifyOne sends the battery for one command and maps the response.
// Failures become the failure verdict (ask, reason naming the failure mode)
// rather than errors, so the batch stays index-aligned with one entry per
// input.
func (b *jevBackend) classifyOne(ctx context.Context, raw string) core.Verdict {
	res, err := b.client.Send(ctx, batteryRequest(raw))
	if err != nil {
		b.mu.Lock()
		b.totalAttempts++
		b.mu.Unlock()
		return failureVerdict(err)
	}
	b.mu.Lock()
	b.totalAttempts += res.Attempts
	if res.Response.Model != "" {
		b.resolvedModel = res.Response.Model
	}
	b.mu.Unlock()
	v, err := mapVerdict(res.Response, b.thresholds)
	if err != nil {
		return failureVerdict(err)
	}
	return v
}

// Classify fans the batch out, one request per command (architecture §5.1),
// with bounded concurrency, and returns one verdict per input, index-aligned
// and order-preserving. Entries the transport or mapping could not judge get
// the failure verdict (ask). An empty batch returns an empty result.
func (b *jevBackend) Classify(ctx context.Context, cmds []core.Command) ([]core.Verdict, error) {
	verdicts := make([]core.Verdict, len(cmds))
	if len(cmds) == 0 {
		return verdicts, nil
	}
	width := b.concurrency
	if width > len(cmds) {
		width = len(cmds)
	}
	jobs := make(chan int)
	wg := sync.WaitGroup{}
	wg.Add(width)
	for w := 0; w < width; w++ {
		go func() {
			defer wg.Done()
			for i := range jobs {
				verdicts[i] = b.classifyOne(ctx, cmds[i].Raw)
			}
		}()
	}
	for i := range cmds {
		select {
		case jobs <- i:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return nil, ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	return verdicts, nil
}

// failureVerdict maps an error onto the conservative failure verdict
// (architecture §5, step 4): ask, with a reason naming the failure mode.
func failureVerdict(err error) core.Verdict {
	return core.Verdict{
		Effect:     core.Ask,
		Confidence: 0,
		Reason:     fmt.Sprintf("jev: command unjudged (%v): defaulting to ask", err),
	}
}

// mapVerdict reads the battery answers and routes them (architecture
// §5bis: thresholds tv1). Answers must carry every hazard noul and the
// severity score; anything missing or malformed is an error mapped to a
// failure verdict by the caller.
func mapVerdict(resp *Response, t Thresholds) (core.Verdict, error) {
	maxHazard, maxHazardIDs := 0.0, []string{}
	for _, h := range hazards {
		a, ok := resp.Answers[h.id]
		if !ok {
			return core.Verdict{}, fmt.Errorf("jev: answer for %q missing", h.id)
		}
		p, err := noulProbability(a)
		if err != nil {
			return core.Verdict{}, fmt.Errorf("jev: answer for %q unusable: %v", h.id, err)
		}
		if p > maxHazard {
			maxHazard = p
			maxHazardIDs = []string{h.id}
		} else if p == maxHazard && p > 0 {
			maxHazardIDs = append(maxHazardIDs, h.id)
		}
	}
	a, ok := resp.Answers[qSeverity]
	if !ok {
		return core.Verdict{}, fmt.Errorf("jev: answer for %q missing", qSeverity)
	}
	severity, levelProbs, err := severityScore(a)
	if err != nil {
		return core.Verdict{}, fmt.Errorf("jev: answer for %q unusable: %v", qSeverity, err)
	}

	d := route(t, maxHazard, severity)
	if d.rule == ruleAllow {
		return core.Verdict{
			Effect:     core.Allow,
			Confidence: maxHazard,
			Reason:     reason(d, maxHazard, severity, maxHazard, t),
		}, nil
	}

	// Categories hold the governing hazard names: every hazard at or above
	// the ask gate (for a severity-decided verdict, at least the max hazard).
	level := d.severityLevel
	if level == 0 {
		level = 2 // hazard-decided: report hazards at or above the ask gate
	}
	categories := []string{}
	for _, h := range hazards {
		if a, ok := resp.Answers[h.id]; ok {
			if p, err := noulProbability(a); err == nil && p >= t.AskHazard {
				categories = append(categories, h.id)
			}
		}
	}
	if len(categories) == 0 {
		categories = maxHazardIDs
	}

	// Confidence is the governing probability: the decisive hazard
	// probability, or — when the severity gate decided — the probability
	// mass at level `level` or above.
	confidence := maxHazard
	if d.rule == ruleSeverityDeny || d.rule == ruleSeverityAsk {
		confidence = 0
		for i := level; i <= 4; i++ {
			confidence += levelProbs[i]
		}
	}
	return core.Verdict{
		Effect:     d.effect,
		Confidence: confidence,
		Categories: categories,
		Reason:     reason(d, maxHazard, severity, confidence, t),
	}, nil
}

// reason is one sentence naming the fired rule and the decisive probability.
func reason(d decision, maxHazard, severity, confidence float64, t Thresholds) string {
	var rule, measure string
	switch d.rule {
	case ruleHazardDeny:
		rule, measure = "hazard deny rule", fmt.Sprintf("max hazard probability %.2f ≥ %.2f", maxHazard, t.DenyHazard)
	case ruleSeverityDeny:
		rule, measure = "severity deny rule", fmt.Sprintf("expected severity %.2f ≥ %.2f (P(≥%d) = %.2f)", severity, t.DenySeverity, d.severityLevel, confidence)
	case ruleHazardAsk:
		rule, measure = "hazard ask rule", fmt.Sprintf("max hazard probability %.2f ≥ %.2f", maxHazard, t.AskHazard)
	case ruleSeverityAsk:
		rule, measure = "severity ask rule", fmt.Sprintf("expected severity %.2f ≥ %.2f (P(≥%d) = %.2f)", severity, t.AskSeverity, d.severityLevel, confidence)
	default:
		rule, measure = "allow rule", fmt.Sprintf("max hazard probability %.2f < %.2f and severity %.2f < %.2f", maxHazard, t.AskHazard, severity, t.AskSeverity)
	}
	return fmt.Sprintf("jev %s fired: %s.", rule, measure)
}

// noulProbability extracts P(true) from a noul answer: the `noul` scalar
// (the live API shape), else the "p" or "true" probability key, else the
// single probability present.
func noulProbability(a Answer) (float64, error) {
	if a.Type != "noul" {
		return 0, fmt.Errorf("type %q, want noul", a.Type)
	}
	if a.Noul != nil {
		return *a.Noul, nil
	}
	for _, k := range []string{"p", "true"} {
		if v, ok := a.Probabilities[k]; ok {
			return v, nil
		}
	}
	if len(a.Probabilities) == 1 {
		for _, v := range a.Probabilities {
			return v, nil
		}
	}
	return 0, errors.New("no noul scalar or probability key p, true, or single value")
}

// severityScore extracts the expected severity (levels 0–4) and the per-level
// probabilities from a score answer. Level probabilities are keyed by level
// number (as the live API returns them: string keys "0".."4"); the answer's
// fractional `score` is used directly when present, else computed as the
// expectation over the distribution.
func severityScore(a Answer) (float64, map[int]float64, error) {
	if a.Type != "score" {
		return 0, nil, fmt.Errorf("type %q, want score", a.Type)
	}
	probs := map[int]float64{}
	for i := 0; i <= 4; i++ {
		if p, ok := a.Probabilities[strconv.Itoa(i)]; ok {
			probs[i] = p
		}
	}
	if len(probs) == 0 {
		return 0, nil, errors.New("no level probabilities 0..4")
	}
	expected := 0.0
	norm := 0.0
	for i, p := range probs {
		expected += float64(i) * p
		norm += p
	}
	if norm > 0 {
		expected /= norm
	}
	return expected, probs, nil
}
