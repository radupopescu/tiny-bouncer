package chat

import (
	"encoding/json"
	"fmt"
	"strings"

	"tinybouncer/internal/core"
)

// Thresholds holds the certainty floors applied to a chat model's self-reported
// effect (architecture §5.3: a chat backend degrades to ask below its
// confidence floor). Constants in code, versioned as ThresholdsVersion.
type Thresholds struct {
	// AllowFloor is the minimum confidence for an allow to stand; below it the
	// verdict degrades to ask (never the other way round).
	AllowFloor float64
	// DenyFloor is the minimum confidence for a deny to stand; below it the
	// verdict degrades to ask.
	DenyFloor float64
}

// DefaultThresholds is the operating point for chat-tv1.
var DefaultThresholds = Thresholds{AllowFloor: 0.5, DenyFloor: 0.5}

// answer is the JSON object a chat backend is asked to produce.
type answer struct {
	Effect     string   `json:"effect"`
	Confidence float64  `json:"confidence"`
	Categories []string `json:"categories"`
	Reason     string   `json:"reason"`
}

// MapAnswer turns one raw model answer into a generic Verdict, applying the
// certainty floors. It returns an error for anything it cannot use (bad JSON,
// an effect outside allow/ask/deny); the caller turns that into the failure
// verdict ask. A returned verdict is never allow when the model did not say
// allow with sufficient confidence.
func MapAnswer(raw []byte, t Thresholds) (core.Verdict, error) {
	var a answer
	if err := json.Unmarshal(raw, &a); err != nil {
		return core.Verdict{}, fmt.Errorf("unusable answer json: %v", err)
	}
	effect := core.Effect(strings.ToLower(strings.TrimSpace(a.Effect)))
	switch effect {
	case core.Allow, core.Ask, core.Deny:
	default:
		return core.Verdict{}, fmt.Errorf("effect %q is not allow, ask or deny", a.Effect)
	}
	confidence := clamp01(a.Confidence)
	reason := strings.TrimSpace(a.Reason)

	switch {
	case effect == core.Allow && confidence < t.AllowFloor:
		return core.Verdict{Effect: core.Ask, Confidence: confidence, Reason: fmt.Sprintf(allowBelowFloor, confidence, t.AllowFloor)}, nil
	case effect == core.Deny && confidence < t.DenyFloor:
		return core.Verdict{Effect: core.Ask, Confidence: confidence, Reason: fmt.Sprintf(denyBelowFloor, confidence, t.DenyFloor)}, nil
	}
	return core.Verdict{
		Effect:     effect,
		Confidence: confidence,
		Categories: filterCategories(a.Categories),
		Reason:     reason,
	}, nil
}

// filterCategories keeps only vocabulary members, preserving order and dropping
// duplicates and empties.
func filterCategories(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	set := categorySet()
	seen := make(map[string]struct{}, len(in))
	var out []string
	for _, c := range in {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, ok := set[c]; !ok {
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
