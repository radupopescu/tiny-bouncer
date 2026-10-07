package decider

// Transport tests: request shape, the retry policy, error classification and
// health. httptest only; no network.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tinybouncer/internal/backend/systemone"
)

// noWait removes the retry delay so the retry loop is exercised instantly.
func noWait(c *client) *client {
	c.sleep = func(context.Context, time.Duration) error { return nil }
	c.rand = func() float64 { return 1.0 } // full-step backoff, deterministic
	return c
}

func TestSendRequestShape(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != SystemOnePath {
			t.Errorf("got %s %s, want POST %s", r.Method, r.URL.Path, SystemOnePath)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		captured, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(answerBody("m", nil, map[int]float64{0: 1.0})))
	}))
	defer srv.Close()

	c := newClient(srv.URL, DefaultModel, 1, time.Second)
	if _, err := c.send(context.Background(), systemone.BatteryRequest("sudo rm -rf /")); err != nil {
		t.Fatalf("send: %v", err)
	}

	var req struct {
		State     map[string]any `json:"state"`
		Model     string         `json:"model"`
		Questions map[string]struct {
			Type         string          `json:"type"`
			Instructions string          `json:"instructions"`
			Criteria     json.RawMessage `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(captured, &req); err != nil {
		t.Fatalf("request is not JSON: %v", err)
	}
	if req.State["command"] != "sudo rm -rf /" {
		t.Errorf("state.command = %v, want the exact command", req.State["command"])
	}
	if req.Model != DefaultModel {
		t.Errorf("model = %q, want %q", req.Model, DefaultModel)
	}
	if len(req.Questions) != len(systemone.Hazards)+1 {
		t.Fatalf("questions = %d, want %d", len(req.Questions), len(systemone.Hazards)+1)
	}
	// A noul question carries the {true,false} criteria object — the shape the
	// Strands Decider schema accepts (no criteria key list needed).
	noul := req.Questions[systemone.Exfiltration]
	if noul.Type != "noul" || noul.Instructions == "" {
		t.Errorf("noul question = %+v", noul)
	}
	var noulCriteria map[string]string
	if err := json.Unmarshal(noul.Criteria, &noulCriteria); err != nil {
		t.Fatalf("noul criteria is not an object: %v", err)
	}
	if noulCriteria["true"] == "" || noulCriteria["false"] == "" {
		t.Errorf("noul criteria = %v, want true and false criteria", noulCriteria)
	}
	// A score question carries the ordered legend as a bare array.
	score := req.Questions[systemone.Severity]
	if score.Type != "score" {
		t.Errorf("severity question type = %q, want score", score.Type)
	}
	var legend []string
	if err := json.Unmarshal(score.Criteria, &legend); err != nil {
		t.Fatalf("score criteria is not an array: %v", err)
	}
	if len(legend) != len(systemone.SeverityLegend) || legend[0] != systemone.SeverityLegend[0] {
		t.Errorf("score legend = %v, want the verbatim severity legend", legend)
	}
}

func TestSendRetriesTransientStatus(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n := atomic.AddInt32(&hits, 1); n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(answerBody("m", nil, map[int]float64{0: 1.0})))
	}))
	defer srv.Close()

	c := noWait(newClient(srv.URL, "m", 3, time.Second))
	if _, err := c.send(context.Background(), systemone.BatteryRequest("git status")); err != nil {
		t.Fatalf("send after two 503s: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}

	// Retries are bounded by the configured attempts.
	c = noWait(newClient(srv.URL, "m", 1, time.Second))
	if _, err := c.send(context.Background(), systemone.BatteryRequest("git status")); err != nil {
		t.Fatalf("send with attempts exhausted: %v", err)
	}
}

func TestSendNonRetryableFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   string
	}{
		{"422", http.StatusUnprocessableEntity, "422"},
		{"500", http.StatusInternalServerError, "500"},
		{"401", http.StatusUnauthorized, "401"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("decider: rejected"))
			}))
			defer srv.Close()
			c := noWait(newClient(srv.URL, "m", 3, time.Second))
			_, err := c.send(context.Background(), systemone.BatteryRequest("git status"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to name status %s", err, tc.want)
			}
			if got := atomic.LoadInt32(&hits); got != 1 {
				t.Errorf("attempts = %d, want 1 (not retryable)", got)
			}
		})
	}
}

func TestSendUnusableBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	c := newClient(srv.URL, "m", 1, time.Second)
	_, err := c.send(context.Background(), systemone.BatteryRequest("git status"))
	if err == nil || !strings.Contains(err.Error(), "unusable response body") {
		t.Fatalf("error = %v, want an unusable-body error", err)
	}
}

func TestSendTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Hold the request past the client's deadline, but bound the wait so
		// httptest.Server.Close cannot block on a handler that never returns.
		select {
		case <-r.Context().Done():
		case <-time.After(300 * time.Millisecond):
		}
	}))
	defer srv.Close()
	c := newClient(srv.URL, "m", 1, 30*time.Millisecond)
	_, err := c.send(context.Background(), systemone.BatteryRequest("git status"))
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want a timeout error", err)
	}
}

func TestSendConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening any more
	c := newClient(url, "m", 1, time.Second)
	_, err := c.send(context.Background(), systemone.BatteryRequest("git status"))
	if err == nil || !strings.Contains(err.Error(), "network error") {
		t.Fatalf("error = %v, want a network error", err)
	}
}

func TestHealthUnauthorizedAndUnparseable(t *testing.T) {
	// A 200 whose body is not JSON is still healthy: the facts are decoration.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>proxy</html>"))
	}))
	defer srv.Close()
	hi, err := newClient(srv.URL, "m", 1, time.Second).health(context.Background())
	if err != nil {
		t.Fatalf("health = %v, want nil for a 200", err)
	}
	if hi.Model != "" {
		t.Errorf("model = %q, want empty for an unparseable body", hi.Model)
	}
}
