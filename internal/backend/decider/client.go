package decider

// HTTP transport for a locally served Strands Decider checkpoint
// (docs/inference.md, "Serve"). The server binds to 127.0.0.1 by default and
// has no authentication, so this client carries no credentials — unlike the
// Jev client, it only needs a base URL. Stdlib only; tests use httptest.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"tinybouncer/internal/backend/systemone"
)

// Endpoint paths of the Strands Decider server.
const (
	SystemOnePath = "/v1/systemone"
	HealthPath    = "/health"
)

const (
	backoffBase = 100 * time.Millisecond
	backoffCap  = 2 * time.Second
	// snippetLimit bounds a response-body excerpt embedded in an error.
	snippetLimit = 200
)

// client posts System One requests to one decider server. Safe for concurrent
// use.
type client struct {
	baseURL     string
	model       string
	maxAttempts int
	timeout     time.Duration
	hc          *http.Client
	// sleep is injectable so tests exercise the retry loop without waiting.
	sleep func(context.Context, time.Duration) error
	rand  func() float64
}

func newClient(baseURL, model string, maxAttempts int, timeout time.Duration) *client {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if timeout <= 0 {
		timeout = DefaultTimeoutMS * time.Millisecond
	}
	return &client{
		baseURL:     strings.TrimRight(baseURL, "/"),
		model:       model,
		maxAttempts: maxAttempts,
		timeout:     timeout,
		hc:          &http.Client{},
		sleep:       sleepCtx,
		rand:        rand.Float64,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// send posts one System One request, retrying the transient statuses (429,
// 502, 503, 504, 529) with exponential backoff plus jitter. 4xx responses, an
// unusable body, and every other status are final.
func (c *client) send(ctx context.Context, req systemone.Request) (*systemone.Response, error) {
	if req.Model == "" {
		req.Model = c.model
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("cannot encode request: %v", err)
	}
	for attempt := 1; ; attempt++ {
		res, retryable, err := c.attempt(ctx, body)
		if err == nil {
			return res, nil
		}
		if !retryable || attempt >= c.maxAttempts {
			return nil, err
		}
		if err := c.sleep(ctx, c.backoff(attempt)); err != nil {
			return nil, fmt.Errorf("retry cancelled: %v", err)
		}
	}
}

// backoff computes the delay before the next attempt: exponential 100 ms ×2
// per attempt, capped at 2 s, with ±50% jitter.
func (c *client) backoff(attempt int) time.Duration {
	d := backoffBase << (attempt - 1)
	if d <= 0 || d > backoffCap {
		d = backoffCap
	}
	return time.Duration(float64(d) * (0.5 + 0.5*c.rand()))
}

// attempt performs one round trip with the per-request timeout applied.
func (c *client) attempt(ctx context.Context, body []byte) (*systemone.Response, bool, error) {
	actx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(actx, http.MethodPost, c.baseURL+SystemOnePath, bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("cannot build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, false, transportError(actx, ctx, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, transportError(actx, ctx, err)
	}

	switch {
	case resp.StatusCode == http.StatusOK:
		// Decode below.
	case retryableStatus(resp.StatusCode):
		return nil, true, fmt.Errorf("retryable failure (%d): %s", resp.StatusCode, snippet(raw))
	case resp.StatusCode == http.StatusUnprocessableEntity:
		return nil, false, fmt.Errorf("invalid request (422): %s", snippet(raw))
	default:
		return nil, false, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, snippet(raw))
	}

	var out systemone.Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false, fmt.Errorf("unusable response body: %v", err)
	}
	return &out, false, nil
}

// healthInfo is the subset of GET /health the backend records.
type healthInfo struct {
	Status string `json:"status"`
	Model  string `json:"model"`
	Device string `json:"device"`
}

// health calls GET /health. A 200 is healthy; the body is read best-effort for
// the model and device facts doctor reports. Any other status, an unreachable
// endpoint, or a timeout is an error.
func (c *client) health(ctx context.Context) (healthInfo, error) {
	actx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(actx, http.MethodGet, c.baseURL+HealthPath, nil)
	if err != nil {
		return healthInfo{}, fmt.Errorf("cannot build health request: %v", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return healthInfo{}, fmt.Errorf("endpoint unreachable: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return healthInfo{}, fmt.Errorf("health check failed (status %d): %s", resp.StatusCode, snippet(raw))
	}
	var hi healthInfo
	_ = json.Unmarshal(raw, &hi) // the facts are decoration, not the health verdict
	return hi, nil
}

// transportError names a timeout as such and everything else as a network
// failure.
func transportError(actx, ctx context.Context, err error) error {
	if errors.Is(actx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("request timed out: %w", err)
	}
	return fmt.Errorf("network error: %w", err)
}

// retryableStatus reports whether a status triggers the retry loop. The list
// matches the Jev client's; the decider server is a local process, so a
// transient failure is rare but worth one more attempt.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, 529,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// snippet collapses a response body onto one line and truncates it. Only
// bodies the server returns are embedded — error text or its own echo of the
// request shape, never the command payload as a whole.
func snippet(body []byte) string {
	s := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, string(body))
	if len(s) > snippetLimit {
		s = s[:snippetLimit] + "…"
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}
