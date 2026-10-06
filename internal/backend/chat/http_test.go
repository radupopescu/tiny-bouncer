package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wiseyolo/internal/backend"
	"wiseyolo/internal/core"
)

// apiBackend builds the api backend pointed at a test server.
func apiBackend(t *testing.T, values map[string]string) backend.Backend {
	t.Helper()
	factory, ok := backend.Lookup("api")
	if !ok {
		t.Fatal("api backend is not registered")
	}
	b, err := factory(backend.Config{Values: values})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	return b
}

// captured records the last chat request for shape assertions.
type captured struct {
	mu   sync.Mutex
	req  chatRequest
	body []byte
}

func (c *captured) set(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.body = b
	_ = json.Unmarshal(b, &c.req)
}

func (c *captured) get() chatRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.req
}

// verdictBody builds a chat completion whose content is the given model JSON.
func verdictBody(model, content string, prompt, completion int) string {
	b, _ := json.Marshal(map[string]any{
		"model": model,
		"choices": []map[string]any{
			{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"},
		},
		"usage": map[string]any{
			"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion,
		},
	})
	return string(b)
}

func TestAPIClassifyBatchAndRequestShape(t *testing.T) {
	cap := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		cap.set(body)
		w.Header().Set("Content-Type", "application/json")
		content := `{"effect":"allow","confidence":0.95,"categories":["vcs_read"],"reason":"read-only"}`
		_, _ = io.WriteString(w, verdictBody("test-model-1", content, 10, 5))
	}))
	defer srv.Close()

	b := apiBackend(t, map[string]string{apiBaseURLEnv: srv.URL, apiModelEnv: "test-model"})
	cmds := []core.Command{{Raw: "git status"}, {Raw: "ls"}}
	got, err := b.Classify(context.Background(), cmds)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("verdicts = %d, want 2", len(got))
	}
	for i, v := range got {
		if v.Effect != core.Allow || v.Confidence != 0.95 {
			t.Errorf("verdict[%d] = %+v, want allow 0.95", i, v)
		}
	}

	req := cap.get()
	if req.Model != "test-model" || req.Temperature != 0 || req.Stream {
		t.Errorf("request = %+v, want model test-model, temperature 0, no stream", req)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[0].Content != Instructions {
		t.Errorf("messages = %+v, want a system message carrying the policy", req.Messages)
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content != "ls" {
		t.Errorf("user message = %+v, want the exact command", req.Messages[1])
	}
	if req.ResponseFormat.Type != "json_schema" || !req.ResponseFormat.JSONSchema.Strict ||
		req.ResponseFormat.JSONSchema.Name != "Verdict" {
		t.Errorf("response_format = %+v, want strict json_schema named Verdict", req.ResponseFormat)
	}
	var schema struct {
		Properties struct {
			Effect struct {
				Enum []string `json:"enum"`
			} `json:"effect"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(req.ResponseFormat.JSONSchema.Schema, &schema); err != nil {
		t.Fatalf("schema not JSON: %v", err)
	}
	if len(schema.Properties.Effect.Enum) != 3 {
		t.Errorf("effect enum = %v, want allow/ask/deny", schema.Properties.Effect.Enum)
	}

	usage := b.(backend.UsageTracker).LastUsage()
	if usage.Requests != 2 || usage.InputTokens != 20 || usage.OutputTokens != 10 {
		t.Errorf("usage = %+v, want 2 requests / 20 in / 10 out", usage)
	}
	if b.Info().Model != "test-model-1" {
		t.Errorf("resolved model = %q, want test-model-1", b.Info().Model)
	}
}

func TestAPIReasoningContentFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"model":"m","choices":[{"message":{"role":"assistant","content":"","reasoning_content":"{\"effect\":\"deny\",\"confidence\":0.8,\"categories\":[\"sudo\"],\"reason\":\"r\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	}))
	defer srv.Close()
	b := apiBackend(t, map[string]string{apiBaseURLEnv: srv.URL, apiModelEnv: "m"})
	got, err := b.Classify(context.Background(), []core.Command{{Raw: "sudo rm"}})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got[0].Effect != core.Deny || len(got[0].Categories) != 1 || got[0].Categories[0] != "sudo" {
		t.Errorf("verdict = %+v, want deny with sudo from reasoning_content", got[0])
	}
}

func TestAPIFailureModesBecomeAsk(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		timeout string
	}{
		{name: "http error", handler: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}},
		{name: "malformed body", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "not json at all")
		}},
		{name: "no choices", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"model":"m","choices":[]}`)
		}},
		{name: "unusable content", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, verdictBody("m", "maybe allow?", 1, 1))
		}},
		{name: "timeout", timeout: "50", handler: func(w http.ResponseWriter, r *http.Request) {
			// Wait for the client to give up, with a bounded fallback so the
			// handler cannot outlive the test server.
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			values := map[string]string{apiBaseURLEnv: srv.URL, apiModelEnv: "m"}
			if tc.timeout != "" {
				values[timeoutEnv] = tc.timeout
			}
			b := apiBackend(t, values)
			got, err := b.Classify(context.Background(), []core.Command{{Raw: "anything"}})
			if err != nil {
				t.Fatalf("Classify returned error %v; failures must be index-aligned ask", err)
			}
			if got[0].Effect != core.Ask || !strings.Contains(got[0].Reason, "api: command unjudged") {
				t.Errorf("verdict = %+v, want ask with an unjudged reason", got[0])
			}
		})
	}
}

func TestAPIHealth(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer healthy.Close()
	b := apiBackend(t, map[string]string{apiBaseURLEnv: healthy.URL, apiModelEnv: "m"})
	if err := b.HealthCheck(context.Background()); err != nil {
		t.Errorf("healthy HealthCheck = %v, want nil", err)
	}

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer broken.Close()
	b = apiBackend(t, map[string]string{apiBaseURLEnv: broken.URL, apiModelEnv: "m"})
	if err := b.HealthCheck(context.Background()); err == nil {
		t.Error("broken HealthCheck = nil, want an error")
	}
}

func TestAPIFactoryRequiresBaseURL(t *testing.T) {
	factory, _ := backend.Lookup("api")
	if _, err := factory(backend.Config{}); err == nil {
		t.Fatal("factory succeeded with no base URL, want a ConfigError")
	} else if _, ok := err.(*ConfigError); !ok {
		t.Fatalf("error type = %T, want *ConfigError", err)
	}
}
