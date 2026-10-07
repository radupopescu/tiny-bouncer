package jev

// HTTP client for the TypeSafe System One API (architecture §5bis transport).
// Transport only: battery construction and verdict mapping arrive in T06.
// Stdlib only; tests never touch the network (httptest only).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the production System One endpoint base.
	DefaultBaseURL = "https://api.typesafe.ai"

	// SystemOnePath is the endpoint path appended to the base URL.
	SystemOnePath = "/v1/systemone"

	// DefaultModel is the model alias sent when no override is configured.
	DefaultModel = "jev-latest"

	// DefaultTimeout is the default per-request context timeout.
	DefaultTimeout = 15000 * time.Millisecond

	// DefaultMaxAttempts is the default total attempts (first try included).
	DefaultMaxAttempts = 3

	backoffBase = 100 * time.Millisecond
	backoffCap  = 2 * time.Second
)

// Config carries the client's transport configuration. Values come from the
// environment by default (loadEnv), or are set directly in tests.
type Config struct {
	BaseURL     string        // endpoint base, no trailing slash
	APIKey      string        // Bearer token
	Model       string        // requested model or alias
	MaxAttempts int           // total attempts, ≥ 1
	Timeout     time.Duration // per-request context timeout
}

// resolveEnv reads configuration with documented precedence:
// TINY_BOUNCER_JEV_BASE_URL → TYPESAFE_ENDPOINT → DefaultBaseURL for the base;
// TINY_BOUNCER_JEV_API_KEY → TYPESAFE_API_KEY for the key.
func resolveEnv(lookup func(string) string) (Config, error) {
	cfg := Config{BaseURL: DefaultBaseURL, APIKey: lookup("TINY_BOUNCER_JEV_API_KEY"), Model: lookup("TINY_BOUNCER_JEV_MODEL"), Timeout: DefaultTimeout}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.APIKey == "" {
		cfg.APIKey = lookup("TYPESAFE_API_KEY")
	}
	if cfg.APIKey == "" {
		return Config{}, &ConfigError{Message: "jev: no API key configured (set TINY_BOUNCER_JEV_API_KEY or TYPESAFE_API_KEY)"}
	}
	if v := lookup("TINY_BOUNCER_JEV_BASE_URL"); v != "" {
		cfg.BaseURL = v
	} else if v := lookup("TYPESAFE_ENDPOINT"); v != "" {
		cfg.BaseURL = v
	}
	if v := lookup("TINY_BOUNCER_TIMEOUT_MS"); v != "" {
		ms, err := strconv.Atoi(v)
		if err != nil || ms <= 0 {
			return Config{}, &ConfigError{Message: fmt.Sprintf("jev: invalid TINY_BOUNCER_TIMEOUT_MS %q", v)}
		}
		cfg.Timeout = time.Duration(ms) * time.Millisecond
	}
	cfg.MaxAttempts = DefaultMaxAttempts
	if v := lookup("TINY_BOUNCER_RETRIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return Config{}, &ConfigError{Message: fmt.Sprintf("jev: invalid TINY_BOUNCER_RETRIES %q", v)}
		}
		cfg.MaxAttempts = n
	}
	return cfg, nil
}

// New builds a client from the process environment. A missing API key is a
// typed ConfigError (callers exit 1 with a clear message).
func New() (*Client, error) { return NewWithLookup(osLookup) }

func osLookup(k string) string { return os.Getenv(k) }

// NewWithLookup builds a client from an explicit config lookup; used by tests.
func NewWithLookup(lookup func(string) string) (*Client, error) {
	cfg, err := resolveEnv(lookup)
	if err != nil {
		return nil, err
	}
	return NewFromConfig(cfg)
}

// NewFromConfig builds a client from an explicit Config.
func NewFromConfig(cfg Config) (*Client, error) {
	if cfg.APIKey == "" {
		return nil, &ConfigError{Message: "jev: no API key configured"}
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	return &Client{
		cfg:   cfg,
		hc:    &http.Client{},
		now:   time.Now,
		rand:  rand.Float64,
		sleep: time.Sleep,
	}, nil
}

// Client sends System One requests with retry behaviour per architecture
// §5bis. Safe for concurrent use.
type Client struct {
	cfg   Config
	hc    *http.Client
	now   func() time.Time
	rand  func() float64
	sleep func(time.Duration)
}

// Result bundles a successful response with the attempt count for meta.
type Result struct {
	Response *Response
	Attempts int
}

// Send posts one System One request and applies the retry/error policy.
// The cfg Model is used unless req.Model is set. Each attempt gets the
// per-request timeout applied to the caller's context.
//
// Never retry: 401 (auth), 422 (invalid request), any other 4xx, or any
// unusable 2xx body. Retry: 429, 502, 503, 504, 529 with exponential backoff
// (100 ms base ×2, capped at 2 s, plus jitter) honouring retry-after.
func (c *Client) Send(ctx context.Context, req Request) (*Result, error) {
	if req.Model == "" {
		req.Model = c.cfg.Model
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, &InvalidRequestError{Status: 0, Message: fmt.Sprintf("jev: cannot encode request: %v", err)}
	}
	attempts := 0
	for {
		attempts++
		res, rerr := c.attempt(ctx, body)
		if rerr == nil {
			return &Result{Response: res, Attempts: attempts}, nil
		}
		retryable := false
		switch rerr.(type) {
		case *RateLimitError, *OverloadedError:
			retryable = true
		}
		if retryable && attempts < c.cfg.MaxAttempts {
			delay := c.backoff(attempts, rerr)
			if err := c.pause(ctx, delay); err != nil {
				return nil, err
			}
			continue
		}
		return nil, rerr
	}
}

// pause sleeps for d unless the context is cancelled or expires first.
// An injected c.sleep (tests) performs the sleep synchronously.
func (c *Client) pause(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return &TimeoutError{Err: ctx.Err()}
	}
	if c.sleep != nil {
		c.sleep(d)
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return &TimeoutError{Err: ctx.Err()}
	}
}

// backoff computes the delay before the next attempt: retry-after wins when
// present, else exponential backoff 100 ms ×2 per attempt, capped at 2 s,
// with ±50% jitter (delay drawn uniformly from 50% to 100% of the step).
func (c *Client) backoff(attempt int, err error) time.Duration {
	if ra, ok := retryAfterOf(err); ok {
		return ra
	}
	d := backoffBase << (attempt - 1)
	if d <= 0 || d > backoffCap {
		d = backoffCap
	}
	return time.Duration(float64(d) * (0.5 + 0.5*c.rand()))
}

// parseRetryAfter parses a Retry-After header value in seconds.
func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	sec, err := strconv.Atoi(v)
	if err != nil || sec < 0 {
		return 0, false
	}
	return time.Duration(sec) * time.Second, true
}

// retryAfterOf extracts the parsed retry-after delay from a retryable error,
// ok=false when the header was absent.
func retryAfterOf(err error) (time.Duration, bool) {
	var rl *RateLimitError
	var ov *OverloadedError
	switch {
	case errors.As(err, &rl):
		return rl.RetryAfter, rl.RetryAfter > 0
	case errors.As(err, &ov):
		return ov.RetryAfter, ov.RetryAfter > 0
	}
	return 0, false
}

// attempt performs one HTTP round trip with the per-request timeout applied.
func (c *Client) attempt(ctx context.Context, body []byte) (*Response, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	url := strings.TrimRight(c.cfg.BaseURL, "/") + SystemOnePath
	httpReq, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, &InvalidRequestError{Message: fmt.Sprintf("jev: cannot build request: %v", err)}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		if attemptCtx.Err() != nil && errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
			return nil, &TimeoutError{Err: err}
		}
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &TimeoutError{Err: err}
		}
		return nil, &NetworkError{Err: err}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		if attemptCtx.Err() != nil && errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
			return nil, &TimeoutError{Err: err}
		}
		return nil, &NetworkError{Err: err}
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, &AuthError{Message: fmt.Sprintf("jev: unauthorized (401): %s", snippet(string(raw), 200))}
	case resp.StatusCode == http.StatusUnprocessableEntity:
		return nil, &InvalidRequestError{
			Status:  422,
			Snippet: snippet(string(raw), 300),
			Message: fmt.Sprintf("jev: invalid request (422): %s", snippet(string(raw), 300)),
		}
	case retryableStatus(resp.StatusCode):
		code := resp.StatusCode
		ra, hasRA := parseRetryAfter(resp.Header.Get("Retry-After"))
		if code == 529 {
			e := &OverloadedError{Status: code, Message: "jev: endpoint overloaded (529)"}
			if hasRA {
				e.RetryAfter = ra
			}
			return nil, e
		}
		e := &RateLimitError{
			Status:  code,
			Message: fmt.Sprintf("jev: retryable failure (%d): %s", code, snippet(string(raw), 200)),
		}
		if hasRA {
			e.RetryAfter = ra
		}
		return nil, e
	case resp.StatusCode >= 400:
		return nil, &NetworkError{Err: fmt.Errorf("jev: unexpected status %d: %s", resp.StatusCode, snippet(string(raw), 200))}
	}

	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &UnusableBodyError{
			Status:  resp.StatusCode,
			Snippet: snippet(string(raw), 300),
			Message: fmt.Sprintf("jev: unusable response body: %v", err),
		}
	}
	return &out, nil
}
