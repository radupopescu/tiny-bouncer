package jev

// Wire types mirroring the TypeSafe System One API, `POST /v1/systemone`.
// Transport only: the judgment battery and verdict mapping live in T06.

import (
	"encoding/json"
	"fmt"
)

// Request is the System One request envelope: one structured state, the
// requested model (possibly an alias), and the questions keyed by id.
type Request struct {
	State     any                 `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]Question `json:"questions"`
}

// Question is the discriminated question union. Its criteria field is
// shape-discriminated by Type on the wire: noul questions carry an object
// `{"true":…,"false":…}`; score questions carry a bare legend array.
type Question struct {
	Type         string   `json:"type"` // "noul" | "score"
	Instructions string   `json:"instructions"`
	True         string   `json:"-"` // noul criterion (true)
	False        string   `json:"-"` // noul criterion (false)
	Score        []string `json:"-"` // score legend, ordered
}

// MarshalJSON emits the discriminated criteria shape.
func (q Question) MarshalJSON() ([]byte, error) {
	m := map[string]any{"type": q.Type, "instructions": q.Instructions}
	switch q.Type {
	case "noul":
		m["criteria"] = map[string]any{"true": q.True, "false": q.False}
	case "score":
		m["criteria"] = q.Score
	default:
		return nil, fmt.Errorf("jev: unknown question type %q", q.Type)
	}
	return json.Marshal(m)
}

// UnmarshalJSON accepts either criteria shape and fills the matching fields.
func (q *Question) UnmarshalJSON(data []byte) error {
	var wire struct {
		Type         string          `json:"type"`
		Instructions string          `json:"instructions"`
		Criteria     json.RawMessage `json:"criteria"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	q.Type, q.Instructions = wire.Type, wire.Instructions
	switch wire.Type {
	case "noul":
		var crit struct {
			True  string `json:"true"`
			False string `json:"false"`
		}
		if err := json.Unmarshal(wire.Criteria, &crit); err != nil {
			return err
		}
		q.True, q.False = crit.True, crit.False
		return nil
	case "score":
		return json.Unmarshal(wire.Criteria, &q.Score)
	default:
		return fmt.Errorf("jev: unknown question type %q", wire.Type)
	}
}

// NoulQuestion asks the probability that the state satisfies a True criterion
// rather than its False counterpart.
type NoulQuestion struct {
	Instructions string
	True, False  string
}

// ScoreQuestion asks for probabilities over an ordered discrete legend
// (for Jev: severity levels 0–4).
type ScoreQuestion struct {
	Instructions string
	Score        []string
}

// Noul and Score build the discriminated Question union from these helpers.
func Noul(instructions, trueCrit, falseCrit string) Question {
	return Question{Type: "noul", Instructions: instructions, True: trueCrit, False: falseCrit}
}

// Score builds a score-typed Question over an ordered legend.
func Score(instructions string, legend []string) Question {
	return Question{Type: "score", Instructions: instructions, Score: legend}
}

// Answer is the discriminated answer union returned per question id.
//   - type "noul": probability that the True criterion holds.
//   - type "choice": a named choice with a probability distribution and the
//     model's own confidence in the selected choice.
//   - type "score": probabilities over the legend with confidence.
type Answer struct {
	Type          string             `json:"type"` // "noul" | "choice" | "score"
	Choice        string             `json:"choice,omitempty"`
	Legend        []string           `json:"legend,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
}

// Response is the System One response envelope. Model is the resolved model
// name (a concrete version, not the alias that was sent).
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

// Usage reports token consumption for the call. Output tokens are free for
// Jev pricing, but both fields are recorded for eval reports.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}
