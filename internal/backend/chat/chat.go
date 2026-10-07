package chat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"tinybouncer/internal/backend"
	"tinybouncer/internal/core"
)

// init registers the two chat backends. Both are optional: they are only used
// when explicitly configured (an API needs an endpoint; afm needs the `fm`
// CLI), so doctor's default all-backends report omits an unconfigured one.
func init() {
	backend.RegisterOptional("api", apiFactory)
	backend.RegisterOptional("afm", afmFactory)
}

const (
	apiBaseURLEnv = "TINY_BOUNCER_API_BASE_URL"
	apiModelEnv   = "TINY_BOUNCER_API_MODEL"
	apiKeyEnv     = "TINY_BOUNCER_API_KEY"
	afmExecEnv    = "TINY_BOUNCER_AFM_EXECUTABLE"
	afmModelEnv   = "TINY_BOUNCER_AFM_MODEL"

	concurrencyEnv = "TINY_BOUNCER_CHAT_CONCURRENCY"
	timeoutEnv     = "TINY_BOUNCER_CHAT_TIMEOUT_MS"
)

const (
	// DefaultAPIModel pins the API backend to Gemma-4-E2B for now (plan T14: no
	// other model is evaluated). The base URL is environment-specific and must
	// be configured.
	DefaultAPIModel      = "gemma-4-e2b-it-qat@q4_k_xl"
	defaultAFMExecutable = "fm"
	defaultAFMModel      = "system"
	defaultConcurrency   = 1
	defaultTimeoutMS     = 30000
)

// result is one transport answer: the raw model JSON, its token usage (zero for
// afm) and the resolved model name when the transport reports one.
type result struct {
	raw   []byte
	usage backend.Usage
	model string
}

// transport is the provider-specific half of a chat backend.
type transport interface {
	classify(ctx context.Context, command string) (result, error)
	health(ctx context.Context) error
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

// apiFactory builds the OpenAI-compatible backend. A missing base URL is a
// config error (the endpoint is environment-specific); the model defaults to
// Gemma-4-E2B.
func apiFactory(cfg backend.Config) (backend.Backend, error) {
	lookup := envLookup(cfg)
	baseURL := strings.TrimSpace(lookup(apiBaseURLEnv))
	if baseURL == "" {
		return nil, configErrf("no endpoint configured: set %s (e.g. http://127.0.0.1:1234/v1)", apiBaseURLEnv)
	}
	model := strings.TrimSpace(lookup(apiModelEnv))
	if model == "" {
		model = DefaultAPIModel
	}
	conc, timeout, err := transportTuning(lookup)
	if err != nil {
		return nil, err
	}
	return newBackend("api", model,
		newHTTPTransport(baseURL, model, strings.TrimSpace(lookup(apiKeyEnv))),
		conc, timeout), nil
}

// afmFactory builds the Apple Foundation Models backend. A missing `fm`
// executable is a config error (so doctor can omit it when unconfigured).
func afmFactory(cfg backend.Config) (backend.Backend, error) {
	lookup := envLookup(cfg)
	executable := strings.TrimSpace(lookup(afmExecEnv))
	if executable == "" {
		executable = defaultAFMExecutable
	}
	if _, err := exec.LookPath(executable); err != nil {
		return nil, configErrf("executable %q not found: %v", executable, err)
	}
	model := strings.TrimSpace(lookup(afmModelEnv))
	if model == "" {
		model = defaultAFMModel
	}
	schemaPath, err := writeAppleSchema()
	if err != nil {
		return nil, configErrf("could not materialise the schema file: %v", err)
	}
	conc, timeout, err := transportTuning(lookup)
	if err != nil {
		return nil, err
	}
	return newBackend("afm", model, &respondTransport{
		executable:   executable,
		model:        model,
		instructions: Instructions,
		schemaPath:   schemaPath,
	}, conc, timeout), nil
}

// transportTuning reads the shared concurrency and timeout overrides.
func transportTuning(lookup func(string) string) (int, time.Duration, error) {
	conc := defaultConcurrency
	if v := lookup(concurrencyEnv); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return 0, 0, configErrf("invalid %s %q", concurrencyEnv, v)
		}
		conc = n
	}
	timeout := defaultTimeoutMS
	if v := lookup(timeoutEnv); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return 0, 0, configErrf("invalid %s %q", timeoutEnv, v)
		}
		timeout = n
	}
	return conc, time.Duration(timeout) * time.Millisecond, nil
}

// writeAppleSchema materialises the guided-generation schema to a mode-0600
// file and returns its path. A deterministic per-user path is used so repeated
// invocations reuse it; the write is atomic (temp + rename).
func writeAppleSchema() (string, error) {
	path := filepath.Join(os.TempDir(), fmt.Sprintf("tinybouncer-afm-verdict-%d.json", os.Getuid()))
	f, err := os.CreateTemp(filepath.Dir(path), "tinybouncer-afm-*.json")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	cleanup := func() { _ = f.Close(); _ = os.Remove(tmp) }
	if _, err := f.WriteString(appleSchema); err != nil {
		cleanup()
		return "", err
	}
	if err := f.Chmod(0o600); err != nil {
		cleanup()
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// chatBackend is the shared Backend implementation for both transports.
type chatBackend struct {
	name        string
	model       string
	transport   transport
	thresholds  Thresholds
	concurrency int
	timeout     time.Duration

	mu            sync.Mutex
	resolvedModel string
	lastUsage     backend.Usage
}

func newBackend(name, model string, tr transport, concurrency int, timeout time.Duration) *chatBackend {
	return &chatBackend{
		name:        name,
		model:       model,
		transport:   tr,
		thresholds:  DefaultThresholds,
		concurrency: concurrency,
		timeout:     timeout,
	}
}

func (b *chatBackend) Name() string { return b.name }

// Info reports the resolved model name once a response has supplied one.
func (b *chatBackend) Info() backend.Info {
	model := b.model
	b.mu.Lock()
	if b.resolvedModel != "" {
		model = b.resolvedModel
	}
	b.mu.Unlock()
	return backend.Info{
		Name:              b.name,
		Model:             model,
		PolicyVersion:     PolicyVersion,
		ThresholdsVersion: ThresholdsVersion,
	}
}

// LastUsage returns the summed usage of the most recent Classify call.
func (b *chatBackend) LastUsage() backend.Usage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastUsage
}

// HealthCheck verifies the transport is reachable, bounded by the backend
// timeout so doctor cannot block indefinitely.
func (b *chatBackend) HealthCheck(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	if err := b.transport.health(cctx); err != nil {
		return fmt.Errorf("%s: %v", b.name, err)
	}
	return nil
}

// Classify runs one transport request per command with bounded concurrency and
// returns index-aligned verdicts. Failures become the conservative failure
// verdict ask, never an error, so a batch stays aligned.
func (b *chatBackend) Classify(ctx context.Context, cmds []core.Command) ([]core.Verdict, error) {
	verdicts := make([]core.Verdict, len(cmds))
	if len(cmds) == 0 {
		return verdicts, nil
	}
	width := b.concurrency
	if width > len(cmds) {
		width = len(cmds)
	}
	b.mu.Lock()
	b.lastUsage = backend.Usage{}
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

func (b *chatBackend) classifyOne(ctx context.Context, command string) core.Verdict {
	cctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	res, err := b.transport.classify(cctx, command)
	if err != nil {
		return b.failure(err)
	}
	b.mu.Lock()
	b.lastUsage.Requests += res.usage.Requests
	b.lastUsage.InputTokens += res.usage.InputTokens
	b.lastUsage.OutputTokens += res.usage.OutputTokens
	if res.model != "" {
		b.resolvedModel = res.model
	}
	b.mu.Unlock()
	v, err := MapAnswer(res.raw, b.thresholds)
	if err != nil {
		return b.failure(err)
	}
	return v
}

// failure maps any transport or parsing error onto the conservative ask
// verdict with a reason naming the backend and the mode.
func (b *chatBackend) failure(err error) core.Verdict {
	return core.Verdict{
		Effect: core.Ask,
		Reason: fmt.Sprintf("%s: command unjudged (%v): defaulting to ask", b.name, err),
	}
}
