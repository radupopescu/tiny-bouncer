// Package chat implements the architecture §10 "prompt-and-parse" backend class
// as two registered backends sharing one policy, verdict schema, mapping and
// certainty discipline:
//
//   - api — any OpenAI-compatible Chat Completions endpoint. It is exercised
//     only with Gemma-4-E2B served by LM Studio for now (see the plan T14).
//   - afm — Apple Foundation Models via the `fm respond --schema` CLI.
//
// Both send the exact command as the user message and expect one JSON object
// {effect, confidence, categories, reason}. Anything malformed, refused, timed
// out or unavailable fails safe to ask; nothing ever widens to allow.
package chat

import "fmt"

// ConfigError is a typed configuration error, returned by a factory when the
// backend cannot be built (missing model, missing executable). The CLI maps it
// to a usage/config exit; doctor reports it without any network call.
type ConfigError struct{ Message string }

func (e *ConfigError) Error() string { return "chat: " + e.Message }

func configErrf(format string, args ...any) error {
	return &ConfigError{Message: fmt.Sprintf(format, args...)}
}
