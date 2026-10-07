package systemone

// Verdict mapping (architecture §5bis): the battery answers in, one generic
// core.Verdict out, with the certainty discipline of architecture §5.3 — a
// deny only above its gates, nothing ever auto-upgraded from deny or ask to
// allow. The label names the backend in the reason sentence.

import (
	"errors"
	"fmt"
	"strconv"

	"tinybouncer/internal/core"
)

// MapVerdict reads the battery answers and routes them. Answers must carry
// every hazard noul and the severity score; anything missing or malformed is
// an error, which the calling backend turns into its failure verdict (ask).
// The label (the backend's name) prefixes the reason sentence.
func MapVerdict(resp *Response, t Thresholds, label string) (core.Verdict, error) {
	maxHazard, maxHazardIDs := 0.0, []string{}
	for _, h := range Hazards {
		a, ok := resp.Answers[h.ID]
		if !ok {
			return core.Verdict{}, fmt.Errorf("answer for %q missing", h.ID)
		}
		p, err := noulProbability(a)
		if err != nil {
			return core.Verdict{}, fmt.Errorf("answer for %q unusable: %v", h.ID, err)
		}
		if p > maxHazard {
			maxHazard = p
			maxHazardIDs = []string{h.ID}
		} else if p == maxHazard && p > 0 {
			maxHazardIDs = append(maxHazardIDs, h.ID)
		}
	}
	a, ok := resp.Answers[Severity]
	if !ok {
		return core.Verdict{}, fmt.Errorf("answer for %q missing", Severity)
	}
	severity, levelProbs, err := severityScore(a)
	if err != nil {
		return core.Verdict{}, fmt.Errorf("answer for %q unusable: %v", Severity, err)
	}

	d := Route(t, maxHazard, severity)
	if d.Rule == RuleAllow {
		return core.Verdict{
			Effect:     core.Allow,
			Confidence: maxHazard,
			Reason:     reason(d, maxHazard, severity, maxHazard, t, label),
		}, nil
	}

	// Categories hold the governing hazard names: every hazard at or above
	// the ask gate (for a severity-decided verdict, at least the max hazard).
	level := d.SeverityLevel
	if level == 0 {
		level = 2 // hazard-decided: report hazards at or above the ask gate
	}
	categories := []string{}
	for _, h := range Hazards {
		if a, ok := resp.Answers[h.ID]; ok {
			if p, err := noulProbability(a); err == nil && p >= t.AskHazard {
				categories = append(categories, h.ID)
			}
		}
	}
	if len(categories) == 0 {
		categories = maxHazardIDs
	}

	// Confidence is the governing probability: the decisive hazard
	// probability, or — when the severity gate decided — the probability
	// mass at level `level` or above.
	confidence := maxHazard
	if d.Rule == RuleSeverityDeny || d.Rule == RuleSeverityAsk {
		confidence = 0
		for i := level; i <= 4; i++ {
			confidence += levelProbs[i]
		}
	}
	return core.Verdict{
		Effect:     d.Effect,
		Confidence: confidence,
		Categories: categories,
		Reason:     reason(d, maxHazard, severity, confidence, t, label),
	}, nil
}

// reason is one sentence naming the fired rule and the decisive probability.
func reason(d Decision, maxHazard, severity, confidence float64, t Thresholds, label string) string {
	var rule, measure string
	switch d.Rule {
	case RuleHazardDeny:
		rule, measure = "hazard deny rule", fmt.Sprintf("max hazard probability %.2f ≥ %.2f", maxHazard, t.DenyHazard)
	case RuleSeverityDeny:
		rule, measure = "severity deny rule", fmt.Sprintf("expected severity %.2f ≥ %.2f (P(≥%d) = %.2f)", severity, t.DenySeverity, d.SeverityLevel, confidence)
	case RuleHazardAsk:
		rule, measure = "hazard ask rule", fmt.Sprintf("max hazard probability %.2f ≥ %.2f", maxHazard, t.AskHazard)
	case RuleSeverityAsk:
		rule, measure = "severity ask rule", fmt.Sprintf("expected severity %.2f ≥ %.2f (P(≥%d) = %.2f)", severity, t.AskSeverity, d.SeverityLevel, confidence)
	default:
		rule, measure = "allow rule", fmt.Sprintf("max hazard probability %.2f < %.2f and severity %.2f < %.2f", maxHazard, t.AskHazard, severity, t.AskSeverity)
	}
	return fmt.Sprintf("%s %s fired: %s.", label, rule, measure)
}

// noulProbability extracts P(true) from a noul answer: the `noul` scalar
// (the live API shape), else the "p" or "true" probability key, else the
// single probability present.
func noulProbability(a Answer) (float64, error) {
	if a.Type != "noul" {
		return 0, fmt.Errorf("type %q, want noul", a.Type)
	}
	if a.Noul != nil {
		return *a.Noul, nil
	}
	for _, k := range []string{"p", "true"} {
		if v, ok := a.Probabilities[k]; ok {
			return v, nil
		}
	}
	if len(a.Probabilities) == 1 {
		for _, v := range a.Probabilities {
			return v, nil
		}
	}
	return 0, errors.New("no noul scalar or probability key p, true, or single value")
}

// severityScore extracts the expected severity (levels 0–4) and the per-level
// probabilities from a score answer. Level probabilities are keyed by level
// number (as the live API returns them: string keys "0".."4"); the expected
// value is computed as the expectation over the distribution.
func severityScore(a Answer) (float64, map[int]float64, error) {
	if a.Type != "score" {
		return 0, nil, fmt.Errorf("type %q, want score", a.Type)
	}
	probs := map[int]float64{}
	for i := 0; i <= 4; i++ {
		if p, ok := a.Probabilities[strconv.Itoa(i)]; ok {
			probs[i] = p
		}
	}
	if len(probs) == 0 {
		return 0, nil, errors.New("no level probabilities 0..4")
	}
	expected := 0.0
	norm := 0.0
	for i, p := range probs {
		expected += float64(i) * p
		norm += p
	}
	if norm > 0 {
		expected /= norm
	}
	return expected, probs, nil
}
