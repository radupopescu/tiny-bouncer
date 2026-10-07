package jev

// The Jev backend proper (architecture §5bis): the shared hazard battery and
// verdict mapping (internal/backend/systemone) wired onto the System One
// transport from the T05 client, registered in the backend registry. One
// request per command, bounded fan-out, fail-safe to ask for entries it
// cannot judge.
//
// Certainty discipline (architecture §5.3) is inherent in the route: a deny
// fires only above its gates, nothing is ever auto-upgraded from deny or ask
// to allow, and thresholds hold only where a gate fired.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"

	"tinybouncer/internal/backend"
	"tinybouncer/internal/backend/systemone"
	"tinybouncer/internal/core"
)

// ModelListPath is the endpoint path used by HealthCheck.
const ModelListPath = "/v1/models"

func init() {
	backend.Register("jev", factory)
}

// DefaultConcurrency is the bounded fan-out width (architecture §5, step 3).
const DefaultConcurrency = 5

// concurrencyEnv overrides the fan-out width.
const concurrencyEnv = "TINY_BOUNCER_CONCURRENCY"

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
	thresholds systemone.Thresholds
	// concurrency bounds the fan-out width, default 5 (TINY_BOUNCER_CONCURRENCY).
	concurrency int

	// resolvedModel is the resolved model name from the most recent
	// successful response (e.g. jev-1.13.0), not the alias sent. It feeds
	// Info() so meta.backend_model reports the resolved model.
	mu            sync.Mutex
	resolvedModel string
	// lastUsage accumulates the Usage records of the most recent Classify
	// call (architecture §5 step 5); LastUsage exposes it to the eval
	// harness. Per Classify the counters reset to zero.
	lastUsage backend.Usage
	// totalAttempts accumulates Send attempt counts for the meta pipeline.
	totalAttempts int
}

// LastUsage returns the usage of the most recent Classify call, zero before
// any call.
func (b *jevBackend) LastUsage() backend.Usage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastUsage
}

func (b *jevBackend) Name() string { return "jev" }

// Info returns the identity facts recorded in meta. After any response has
// resolved, Model is the response's model field; before then, the configured
// model (TINY_BOUNCER_JEV_MODEL, default jev-latest).
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
		PolicyVersion:     systemone.PolicyVersion,
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
		return fmt.Errorf("jev: health check failed (401): check TINY_BOUNCER_JEV_API_KEY or TYPESAFE_API_KEY")
	}
	return fmt.Errorf("jev: health check failed (status %d): endpoint returned an error", res.StatusCode)
}

// classifyOne sends the battery for one command and maps the response.
// Failures become the failure verdict (ask, reason naming the failure mode)
// rather than errors, so the batch stays index-aligned with one entry per
// input.
func (b *jevBackend) classifyOne(ctx context.Context, raw string) core.Verdict {
	res, err := b.client.Send(ctx, systemone.BatteryRequest(raw))
	if err != nil {
		b.mu.Lock()
		b.totalAttempts++
		b.mu.Unlock()
		return failureVerdict(err)
	}
	b.mu.Lock()
	b.totalAttempts += res.Attempts
	b.lastUsage.Requests++
	b.lastUsage.InputTokens += res.Response.Usage.InputTokens
	b.lastUsage.OutputTokens += res.Response.Usage.OutputTokens
	if res.Response.Model != "" {
		b.resolvedModel = res.Response.Model
	}
	b.mu.Unlock()
	v, err := systemone.MapVerdict(res.Response, b.thresholds, "jev")
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
	b.mu.Lock()
	b.lastUsage = backend.Usage{} // a new call reports its own usage only
	b.mu.Unlock()
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
