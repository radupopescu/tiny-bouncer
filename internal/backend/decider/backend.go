package decider

// The decider backend (architecture §5quinquies): the shared System One
// battery and route (internal/backend/systemone) wired onto a locally served
// Strands Decider checkpoint. The server is an external process — the Python
// package is not vendored into this repository, exactly as LM Studio is not
// for the `api` backend. One request per command, bounded fan-out, fail-safe
// to ask for entries it cannot judge.
//
// The backend is optional: it is only used when an endpoint is configured, so
// doctor's default all-backends report omits it otherwise.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"tinybouncer/internal/backend"
	"tinybouncer/internal/backend/systemone"
	"tinybouncer/internal/core"
)

// Environment variables.
const (
	baseURLEnv     = "TINY_BOUNCER_DECIDER_BASE_URL"
	modelEnv       = "TINY_BOUNCER_DECIDER_MODEL"
	timeoutEnv     = "TINY_BOUNCER_DECIDER_TIMEOUT_MS"
	retriesEnv     = "TINY_BOUNCER_DECIDER_RETRIES"
	concurrencyEnv = "TINY_BOUNCER_DECIDER_CONCURRENCY"
)

// Defaults. The model alias is the one the Strands Decider server documents;
// the server resolves the checkpoint it serves and the response's model field
// is what gets recorded.
const (
	DefaultModel       = "strands-decider-latest"
	DefaultTimeoutMS   = 30000
	DefaultMaxAttempts = 3
	DefaultConcurrency = 1
)

func init() {
	backend.RegisterOptional("decider", factory)
}

// configErrf builds the typed configuration error every backend shares.
func configErrf(format string, args ...any) error {
	return &systemone.ConfigError{Message: "decider: " + fmt.Sprintf(format, args...)}
}

// factory builds the backend from backend.Config.Values falling back to the
// process environment. A missing base URL is a typed config error (so doctor
// omits an unconfigured decider); so is an invalid numeric override or a
// malformed threshold override.
func factory(cfg backend.Config) (backend.Backend, error) {
	lookup := envLookup(cfg)
	baseURL := strings.TrimSpace(lookup(baseURLEnv))
	if baseURL == "" {
		return nil, configErrf("no endpoint configured: set %s (e.g. http://127.0.0.1:8000)", baseURLEnv)
	}
	model := strings.TrimSpace(lookup(modelEnv))
	if model == "" {
		model = DefaultModel
	}
	concurrency, err := positiveInt(lookup, concurrencyEnv, DefaultConcurrency)
	if err != nil {
		return nil, err
	}
	timeoutMS, err := positiveInt(lookup, timeoutEnv, DefaultTimeoutMS)
	if err != nil {
		return nil, err
	}
	attempts, err := positiveInt(lookup, retriesEnv, DefaultMaxAttempts)
	if err != nil {
		return nil, err
	}
	thresholds, err := LoadThresholds(lookup)
	if err != nil {
		return nil, err
	}
	return &deciderBackend{
		client:      newClient(baseURL, model, attempts, time.Duration(timeoutMS)*time.Millisecond),
		model:       model,
		thresholds:  thresholds,
		concurrency: concurrency,
	}, nil
}

// envLookup prefers a backend.Config override, then the process environment.
func envLookup(cfg backend.Config) func(string) string {
	return func(key string) string {
		if cfg.Values != nil {
			if v := cfg.Values[key]; v != "" {
				return v
			}
		}
		return os.Getenv(key)
	}
}

// positiveInt reads an optional positive integer override.
func positiveInt(lookup func(string) string, key string, fallback int) (int, error) {
	v := strings.TrimSpace(lookup(key))
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, configErrf("invalid %s %q", key, v)
	}
	return n, nil
}

// deciderBackend implements backend.Backend for a Strands Decider server.
// Safe for concurrent use.
type deciderBackend struct {
	client      *client
	model       string
	thresholds  systemone.Thresholds
	concurrency int

	mu sync.Mutex
	// resolvedModel is the model field of the most recent successful response
	// (the checkpoint the server actually served), not the alias sent.
	resolvedModel string
	// healthModel is the model GET /health reported, so doctor can name the
	// checkpoint before any classification has run.
	healthModel string
	// lastUsage accumulates the usage of the most recent Classify call.
	lastUsage backend.Usage
}

func (b *deciderBackend) Name() string { return "decider" }

// SweepEnv names the threshold override variable `eval --sweep` varies
// (backend.Sweepable).
func (b *deciderBackend) SweepEnv() string { return thresholdsEnv }

// LastUsage returns the usage of the most recent Classify call, zero before any
// call (architecture §5 step 5).
func (b *deciderBackend) LastUsage() backend.Usage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastUsage
}

// Info returns the identity facts recorded in meta: the resolved response
// model once one has arrived, else the model health reported, else the
// configured alias.
func (b *deciderBackend) Info() backend.Info {
	model := b.model
	b.mu.Lock()
	switch {
	case b.resolvedModel != "":
		model = b.resolvedModel
	case b.healthModel != "":
		model = b.healthModel
	}
	b.mu.Unlock()
	return backend.Info{
		Name:              "decider",
		Model:             model,
		PolicyVersion:     systemone.PolicyVersion,
		ThresholdsVersion: ThresholdsVersion,
	}
}

// HealthCheck calls GET /health and records the model it reports.
func (b *deciderBackend) HealthCheck(ctx context.Context) error {
	hi, err := b.client.health(ctx)
	if err != nil {
		return fmt.Errorf("decider: %v", err)
	}
	if hi.Model != "" {
		b.mu.Lock()
		b.healthModel = hi.Model
		b.mu.Unlock()
	}
	return nil
}

// Classify fans the batch out, one request per command, with bounded
// concurrency and one verdict per input, index-aligned and order-preserving.
// Entries the transport or mapping could not judge get the failure verdict
// (ask). An empty batch returns an empty result.
func (b *deciderBackend) Classify(ctx context.Context, cmds []core.Command) ([]core.Verdict, error) {
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
	var wg sync.WaitGroup
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

// classifyOne sends the shared battery for one command and maps the response.
// Failures become the failure verdict rather than errors, so the batch stays
// index-aligned.
func (b *deciderBackend) classifyOne(ctx context.Context, raw string) core.Verdict {
	cctx, cancel := context.WithTimeout(ctx, b.client.timeout)
	defer cancel()
	resp, err := b.client.send(cctx, systemone.BatteryRequest(raw))
	if err != nil {
		return b.failure(err)
	}
	b.mu.Lock()
	b.lastUsage.Requests++
	b.lastUsage.InputTokens += resp.Usage.InputTokens
	b.lastUsage.OutputTokens += resp.Usage.OutputTokens
	if resp.Model != "" {
		b.resolvedModel = resp.Model
	}
	b.mu.Unlock()
	v, err := systemone.MapVerdict(resp, b.thresholds, "decider")
	if err != nil {
		return b.failure(err)
	}
	return v
}

// failure maps any transport or mapping error onto the conservative failure
// verdict (architecture §5, step 4): ask, with a reason naming the mode.
func (b *deciderBackend) failure(err error) core.Verdict {
	return core.Verdict{
		Effect: core.Ask,
		Reason: fmt.Sprintf("decider: command unjudged (%v): defaulting to ask", err),
	}
}
