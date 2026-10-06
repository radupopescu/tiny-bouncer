package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"wiseyolo/internal/backend"
)

// maxResponseBytes bounds a model answer so a misbehaving server cannot exhaust
// memory.
const maxResponseBytes = 1 << 20

// httpTransport speaks the OpenAI Chat Completions API (architecture §10): a
// strictly schema-constrained single completion per command. It is the `api`
// backend's transport and works with any compatible endpoint (LM Studio,
// Ollama, vLLM, a hosted API).
type httpTransport struct {
	baseURL string
	model   string
	apiKey  string
	client  *http.Client
}

func newHTTPTransport(baseURL, model, apiKey string) *httpTransport {
	return &httpTransport{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		apiKey:  apiKey,
		client:  &http.Client{},
	}
}

type chatRequest struct {
	Model          string         `json:"model"`
	Messages       []chatMessage  `json:"messages"`
	Temperature    float64        `json:"temperature"`
	Stream         bool           `json:"stream"`
	ResponseFormat responseFormat `json:"response_format"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type       string         `json:"type"`
	JSONSchema jsonSchemaSpec `json:"json_schema"`
}

type jsonSchemaSpec struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content          string  `json:"content"`
			ReasoningContent string  `json:"reasoning_content"`
			Refusal          *string `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func (t *httpTransport) classify(ctx context.Context, command string) (result, error) {
	body, err := json.Marshal(chatRequest{
		Model: t.model,
		Messages: []chatMessage{
			{Role: "system", Content: Instructions},
			{Role: "user", Content: command},
		},
		Temperature: 0,
		Stream:      false,
		ResponseFormat: responseFormat{
			Type: "json_schema",
			JSONSchema: jsonSchemaSpec{
				Name:   "Verdict",
				Strict: true,
				Schema: jsonMessage(httpSchema),
			},
		},
	})
	if err != nil {
		return result{}, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return result{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if t.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
	}
	res, err := t.client.Do(req)
	if err != nil {
		return result{}, fmt.Errorf("request failed: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if res.StatusCode != http.StatusOK {
		return result{}, fmt.Errorf("http %d: %s", res.StatusCode, snippet(raw))
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return result{}, fmt.Errorf("unusable response body: %v", err)
	}
	if len(cr.Choices) == 0 {
		return result{}, fmt.Errorf("response carried no choices")
	}
	msg := cr.Choices[0].Message
	content := strings.TrimSpace(msg.Content)
	if content == "" {
		// Reasoning models (e.g. some LM Studio models) leave content empty
		// and put the answer in reasoning_content.
		content = strings.TrimSpace(msg.ReasoningContent)
	}
	if content == "" {
		if msg.Refusal != nil && strings.TrimSpace(*msg.Refusal) != "" {
			return result{}, fmt.Errorf("model refused: %s", snippet([]byte(*msg.Refusal)))
		}
		return result{}, fmt.Errorf("response carried no content")
	}
	return result{
		raw: []byte(content),
		usage: backend.Usage{
			Requests:     1,
			InputTokens:  cr.Usage.PromptTokens,
			OutputTokens: cr.Usage.CompletionTokens,
		},
		model: cr.Model,
	}, nil
}

func (t *httpTransport) health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.baseURL+"/models", nil)
	if err != nil {
		return fmt.Errorf("build health request: %w", err)
	}
	if t.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
	}
	res, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("endpoint unreachable: %v", err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode == http.StatusOK {
		return nil
	}
	return fmt.Errorf("model list failed (status %d): check the base URL and that the server is running", res.StatusCode)
}

// snippet trims a body to a short single-line fragment for error messages.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
