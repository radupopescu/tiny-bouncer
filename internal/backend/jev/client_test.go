package jev

// Unit tests for the System One HTTP client. httptest.Server only — no test
// in this package touches the network.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// recordingClient builds a client whose retry sleeps (and jitter) are
// captured rather than performed, so tests stay fast and assert delays.
func recordingClient(cfg Config) (*Client, *[]time.Duration) {
	c, err := NewFromConfig(cfg)
	if err != nil {
		panic(err)
	}
	sleeps := &[]time.Duration{}
	c.sleep = func(d time.Duration) { *sleeps = append(*sleeps, d) }
	c.rand = func() float64 { return 0.5 } // jitter = 75% of step
	return c, sleeps
}

// captureHandler returns a handler counting requests and recording the last
// raw request body it received.
func captureHandler(t *testing.T, status func(int) int) (*httptest.Server, *int32, *string) {
	var hits int32
	var lastBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		lastBody = string(body)
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("Authorization header = %q", r.Header.Get("Authorization"))
		}
		if r.Method != http.MethodPost || r.URL.Path != SystemOnePath {
			t.Errorf("got %s %s, want POST %s", r.Method, r.URL.Path, SystemOnePath)
		}
		w.WriteHeader(status(int(n)))
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"hazard":{"type":"noul","probabilities":{"p":0.1}}},"usage":{"input_tokens":401,"output_tokens":0}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &lastBody
}

func testCfg(baseURL string, attempts int) Config {
	return Config{
		BaseURL:     baseURL,
		APIKey:      "test-key",
		Model:       "jev-latest",
		MaxAttempts: attempts,
		Timeout:     DefaultTimeout,
	}
}

func sampleRequest() Request {
	return Request{
		State: map[string]any{"command": "sudo rm -rf /"},
		Model: "jev-latest",
		Questions: map[string]Question{
			"destructive_data": {
				Type:         "noul",
				Instructions: "Estimate the probability.",
				True:         "It destroys data.",
				False:        "It destroys nothing.",
			},
			"severity": {
				Type:         "score",
				Instructions: "Assess severity.",
				Score:        []string{"0 none", "1 low", "2 moderate", "3 high", "4 catastrophic"},
			},
		},
	}
}

func TestSendRequestShape(t *testing.T) {
	// The wire format is asserted here, exactly, against the captured
	// example — and nowhere else (architecture plan T05).
	want := map[string]any{
		"state": map[string]any{"command": "sudo rm -rf /"},
		"model": "jev-latest",
		"questions": map[string]any{
			"destructive_data": map[string]any{
				"type":         "noul",
				"instructions": "Estimate the probability.",
				"criteria": map[string]any{
					"true":  "It destroys data.",
					"false": "It destroys nothing.",
				},
			},
			"severity": map[string]any{
				"type":         "score",
				"instructions": "Assess severity.",
				"criteria":     []any{"0 none", "1 low", "2 moderate", "3 high", "4 catastrophic"},
			},
		},
	}
	req := sampleRequest()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("request wire shape mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestSendRoundTrip(t *testing.T) {
	srv, _, lastBody := captureHandler(t, func(int) int { return http.StatusOK })
	c, _ := recordingClient(testCfg(srv.URL, 3))
	req := sampleRequest()
	res, err := c.Send(context.Background(), req)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Response == nil || res.Response.Model != "jev-1.13.0" {
		t.Fatalf("unexpected response: %+v", res.Response)
	}
	ans, ok := res.Response.Answers["hazard"]
	if !ok || ans.Type != "noul" {
		t.Fatalf("answers = %+v", res.Response.Answers)
	}
	if res.Response.Usage != (Usage{InputTokens: 401, OutputTokens: 0}) {
		t.Fatalf("usage = %+v", res.Response.Usage)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(*lastBody), &sent); err != nil {
		t.Fatalf("parse captured body: %v", err)
	}
	if sent["model"] != "jev-latest" {
		t.Fatalf("captured model = %v", sent["model"])
	}
	if res.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", res.Attempts)
	}
}

func TestSendModelDefaultsToConfigured(t *testing.T) {
	srv, _, lastBody := captureHandler(t, func(int) int { return http.StatusOK })
	cfg := testCfg(srv.URL, 1)
	cfg.Model = ""
	c, _ := recordingClient(cfg)
	if _, err := c.Send(context.Background(), Request{State: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(*lastBody), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["model"] != DefaultModel {
		t.Fatalf("model = %v, want %q", sent["model"], DefaultModel)
	}
}

func Test401NoRetry(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer srv.Close()
	c, sleeps := recordingClient(testCfg(srv.URL, 5))
	_, err := c.Send(context.Background(), Request{State: map[string]any{}})
	if err == nil {
		t.Fatal("expected error")
	}
	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("got %T (%v), want *AuthError", err, err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits = %d, want 1 (401 must not retry)", got)
	}
	if len(*sleeps) != 0 {
		t.Fatalf("sleeps = %v, want none", *sleeps)
	}
}

func Test429HonoursRetryAfter(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		if n > 3 {
			_, _ = fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()
	c, sleeps := recordingClient(testCfg(srv.URL, 4))
	_, err := c.Send(context.Background(), Request{State: map[string]any{}})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := atomic.LoadInt32(&hits); got != 4 {
		t.Fatalf("hits = %d, want 4", got)
	}
	want := []time.Duration{7 * time.Second, 7 * time.Second, 7 * time.Second}
	if !reflect.DeepEqual(*sleeps, want) {
		t.Fatalf("sleeps = %v, want %v (retry-after must win)", *sleeps, want)
	}
}

func Test429ThenSuccessAttemptsRecorded(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"model":"m","answers":{},"usage":{"input_tokens":5,"output_tokens":0}}`))
	}))
	defer srv.Close()
	c, _ := recordingClient(testCfg(srv.URL, 3))
	res, err := c.Send(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", res.Attempts)
	}
}

func Test529Retried(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(529)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"model":"m","answers":{},"usage":{"input_tokens":5,"output_tokens":0}}`))
	}))
	defer srv.Close()
	c, _ := recordingClient(testCfg(srv.URL, 3))
	res, err := c.Send(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3", res.Attempts)
	}
}

func Test502ExhaustsAttempts(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, sleeps := recordingClient(testCfg(srv.URL, 3))
	_, err := c.Send(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected error")
	}
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("got %T (%v), want *RateLimitError", err, err)
	}
	if rl.Status != http.StatusBadGateway {
		t.Fatalf("status = %d", rl.Status)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("hits = %d, want 3 (exhausted)", got)
	}
	// Exponential backoff 100 ms ×2 vs jitter 75%: 75 ms, 150 ms.
	want := []time.Duration{75 * time.Millisecond, 150 * time.Millisecond}
	if !reflect.DeepEqual(*sleeps, want) {
		t.Fatalf("sleeps = %v, want %v", *sleeps, want)
	}
}

func Test422SnippetNoRetry(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte("validation failed: state must be an object\nand this line explains more"))
	}))
	defer srv.Close()
	c, _ := recordingClient(testCfg(srv.URL, 5))
	_, err := c.Send(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected error")
	}
	var ire *InvalidRequestError
	if !errors.As(err, &ire) {
		t.Fatalf("got %T (%v), want *InvalidRequestError", err, err)
	}
	if !strings.Contains(ire.Snippet, "state must be an object") {
		t.Fatalf("snippet = %q, want body excerpt", ire.Snippet)
	}
	if strings.Contains(ire.Snippet, "\n") {
		t.Fatalf("snippet = %q, must be single-line", ire.Snippet)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits = %d, want 1 (422 must not retry)", got)
	}
}

func Test422SnippetTruncated(t *testing.T) {
	ire := &InvalidRequestError{Snippet: snippet(strings.Repeat("x", 500), 300)}
	if n := utf8.RuneCountInString(ire.Snippet); !utf8.ValidString(ire.Snippet) || n != 301 || !strings.HasSuffix(ire.Snippet, "…") {
		t.Fatalf("snippet = %d runes, want 300 + ellipsis", n)
	}
}

func TestOther4xxNoRetry(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c, _ := recordingClient(testCfg(srv.URL, 4))
	_, err := c.Send(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
}

func TestTimeoutAbortsRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(300 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	cfg := testCfg(srv.URL, 2)
	cfg.Timeout = 30 * time.Millisecond
	c, _ := recordingClient(cfg)
	_, err := c.Send(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected error")
	}
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("got %T (%v), want *TimeoutError", err, err)
	}
}

func TestMalformedBodyTypedError(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>proxy error</html>"))
	}))
	defer srv.Close()
	c, _ := recordingClient(testCfg(srv.URL, 4))
	_, err := c.Send(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected error")
	}
	var ue *UnusableBodyError
	if !errors.As(err, &ue) {
		t.Fatalf("got %T (%v), want *UnusableBodyError", err, err)
	}
	if !strings.Contains(ue.Snippet, "proxy error") {
		t.Fatalf("snippet = %q", ue.Snippet)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits = %d, want 1 (2xx body failure must not retry)", got)
	}
}

func TestEnvPrecedence(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		wantBase string
		wantErr  bool
	}{
		{name: "default", env: map[string]string{"TINY_BOUNCER_JEV_API_KEY": "a"}, wantBase: DefaultBaseURL},
		{name: "typesafe endpoint fallback", env: map[string]string{"TINY_BOUNCER_JEV_API_KEY": "a", "TYPESAFE_ENDPOINT": "https://cookbook.typesafe.ai"}, wantBase: "https://cookbook.typesafe.ai"},
		{name: "tiny bouncer override wins", env: map[string]string{"TINY_BOUNCER_JEV_API_KEY": "a", "TYPESAFE_ENDPOINT": "https://cookbook.typesafe.ai", "TINY_BOUNCER_JEV_BASE_URL": "https://override.typesafe.ai"}, wantBase: "https://override.typesafe.ai"},
		{name: "typesafe key fallback", env: map[string]string{"TYPESAFE_API_KEY": "a"}, wantBase: DefaultBaseURL},
		{name: "no key", env: map[string]string{}, wantErr: true},
	}
	// The process environment may carry a live credential (a developer or
	// CI machine with TYPESAFE_API_KEY set); the no-key case requires the
	// candidates to be genuinely absent, so clear them explicitly. Empty
	// counts as unconfigured.
	t.Setenv("TINY_BOUNCER_JEV_API_KEY", "")
	t.Setenv("TYPESAFE_API_KEY", "")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			c, err := New() // Read from the process environment.
			if tt.wantErr {
				var ce *ConfigError
				if !errors.As(err, &ce) {
					t.Fatalf("got %T (%v), want *ConfigError", err, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.cfg.BaseURL != tt.wantBase {
				t.Fatalf("base = %q, want %q", c.cfg.BaseURL, tt.wantBase)
			}
		})
	}
}

func TestKeyPrecedence(t *testing.T) {
	t.Run("tiny bouncer key wins", func(t *testing.T) {
		c, err := NewWithLookup(func(k string) string {
			if k == "TINY_BOUNCER_JEV_API_KEY" {
				return "primary"
			}
			if k == "TYPESAFE_API_KEY" {
				return "fallback"
			}
			return ""
		})
		if err != nil {
			t.Fatal(err)
		}
		if c.cfg.APIKey != "primary" {
			t.Fatalf("key = %q, want primary", c.cfg.APIKey)
		}
	})
	t.Run("typesafe key used otherwise", func(t *testing.T) {
		c, err := NewWithLookup(func(k string) string {
			if k == "TYPESAFE_API_KEY" {
				return "fallback"
			}
			return ""
		})
		if err != nil {
			t.Fatal(err)
		}
		if c.cfg.APIKey != "fallback" {
			t.Fatalf("key = %q, want fallback", c.cfg.APIKey)
		}
	})
	t.Run("missing key typed error", func(t *testing.T) {
		_, err := NewWithLookup(func(string) string { return "" })
		var ce *ConfigError
		if !errors.As(err, &ce) {
			t.Fatalf("got %T (%v), want *ConfigError", err, err)
		}
	})
}

func TestEnvOverrides(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		c, err := NewWithLookup(func(k string) string {
			switch k {
			case "TINY_BOUNCER_JEV_API_KEY":
				return "primary"
			case "TINY_BOUNCER_TIMEOUT_MS":
				return "2500"
			}
			return ""
		})
		if err != nil {
			t.Fatal(err)
		}
		if c.cfg.Timeout != 2500*time.Millisecond {
			t.Fatalf("timeout = %v", c.cfg.Timeout)
		}
	})
	t.Run("retries", func(t *testing.T) {
		c, err := NewWithLookup(func(k string) string {
			switch k {
			case "TINY_BOUNCER_JEV_API_KEY":
				return "primary"
			case "TINY_BOUNCER_RETRIES":
				return "5"
			}
			return ""
		})
		if err != nil {
			t.Fatal(err)
		}
		if c.cfg.MaxAttempts != 5 {
			t.Fatalf("max attempts = %d", c.cfg.MaxAttempts)
		}
	})
	t.Run("bad retries typed error", func(t *testing.T) {
		_, err := NewWithLookup(func(k string) string {
			if k == "TINY_BOUNCER_JEV_API_KEY" {
				return "primary"
			}
			if k == "TINY_BOUNCER_RETRIES" {
				return "zero"
			}
			return ""
		})
		var ce *ConfigError
		if !errors.As(err, &ce) {
			t.Fatalf("got %T (%v), want *ConfigError", err, err)
		}
	})
}

func TestContextCancellationPauses(t *testing.T) {
	c, err := NewFromConfig(testCfg(DefaultBaseURL, 3))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.pause(ctx, DefaultTimeout); err == nil {
		t.Fatal("expected error")
	} else {
		var te *TimeoutError
		if !errors.As(err, &te) {
			t.Fatalf("got %T (%v), want *TimeoutError", err, err)
		}
	}
}
