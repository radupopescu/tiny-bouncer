// Cross-backend comparison (architecture §7, T15): given two stored eval
// reports over the same labelled corpus, Compare aligns them by record id,
// counts the 3x3 verdict-agreement matrix, lists the disagreeing records, and
// isolates the safety-critical cases where one side auto-allowed a command the
// truth judges ask/deny while the other side flagged it. Everything here is a
// pure function over two Report values, so it is unit-testable without a run.
package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"wiseyolo/internal/core"
)

// Provenance identifies one report inside a comparison.
type Provenance struct {
	TS                string `json:"ts"`
	Backend           string `json:"backend"`
	BackendModel      string `json:"backend_model"`
	PolicyVersion     string `json:"policy_version"`
	ThresholdsVersion string `json:"thresholds_version"`
	CorpusSize        int    `json:"corpus_size"`
}

// provenanceOf reduces a report to its identity fields.
func provenanceOf(rep Report) Provenance {
	return Provenance{
		TS:                rep.TS,
		Backend:           rep.Backend,
		BackendModel:      rep.BackendModel,
		PolicyVersion:     rep.PolicyVersion,
		ThresholdsVersion: rep.ThresholdsVersion,
		CorpusSize:        rep.CorpusSize,
	}
}

// EffectCounts is one row or one column of the 3x3 agreement matrix.
type EffectCounts struct {
	Allow int `json:"allow"`
	Ask   int `json:"ask"`
	Deny  int `json:"deny"`
}

// Matrix is the 3x3 agreement matrix: rows are the current report's effects,
// columns the other report's effects, so the diagonal counts agreement.
type Matrix struct {
	Allow EffectCounts `json:"allow"`
	Ask   EffectCounts `json:"ask"`
	Deny  EffectCounts `json:"deny"`
}

// Disagreement is one id-aligned record on which the two verdicts differ.
type Disagreement struct {
	ID      string `json:"id"`
	Truth   string `json:"truth"`
	Current string `json:"current"`
	Other   string `json:"other"`
}

// SafetyCriticalRecord is one record where exactly one side auto-allowed a
// command whose truth is ask or deny, while the other side flagged it. The
// fields name both the verdicts and the backends responsible, so the report
// reads without cross-referencing the provenance block.
type SafetyCriticalRecord struct {
	ID             string `json:"id"`
	Truth          string `json:"truth"`
	AllowedBy      string `json:"allowed_by"`
	FlaggedBy      string `json:"flagged_by"`
	AllowedVerdict string `json:"allowed_verdict"`
	FlaggedVerdict string `json:"flagged_verdict"`
}

// SafetyCritical summarises the asymmetry that matters most: a command one
// backend auto-allowed while the other flagged it and the corpus says it is
// not safe. The two counts are directional (current side / other side), and
// Records lists every such case in current-report order.
type SafetyCritical struct {
	CurrentAllowsOtherFlags int                    `json:"current_allows_other_flags"`
	OtherAllowsCurrentFlags int                    `json:"other_allows_current_flags"`
	Records                 []SafetyCriticalRecord `json:"records"`
}

// Comparison is the full cross-backend comparison of two reports.
type Comparison struct {
	TS             string         `json:"ts"`
	Current        Provenance     `json:"current"`
	Other          Provenance     `json:"other"`
	Deltas         []Delta        `json:"deltas"`
	Matrix         Matrix         `json:"matrix"`
	Disagreements  []Disagreement `json:"disagreements"`
	OnlyInCurrent  []string       `json:"only_in_current,omitempty"`
	OnlyInOther    []string       `json:"only_in_other,omitempty"`
	SafetyCritical SafetyCritical `json:"safety_critical"`
}

// effectIndex maps an effect string onto the matrix ordering allow/ask/deny.
func effectIndex(v string) (int, bool) {
	switch core.Effect(v) {
	case core.Allow:
		return 0, true
	case core.Ask:
		return 1, true
	case core.Deny:
		return 2, true
	}
	return 0, false
}

// effectAt returns the effect name for a matrix index.
func effectAt(i int) string {
	return [...]string{string(core.Allow), string(core.Ask), string(core.Deny)}[i]
}

// dangerousTruth reports whether a truth value is not safe to auto-run.
func dangerousTruth(t string) bool {
	return core.Effect(t) == core.Ask || core.Effect(t) == core.Deny
}

// Compare aligns two reports by record id and returns their comparison.
// current is the run just made; other is the report it is judged against.
// Metric deltas are current minus other. Records present in only one report
// are listed but excluded from the matrix and from the safety-critical tally.
func Compare(current, other Report) Comparison {
	c := Comparison{
		TS:      current.TS,
		Current: provenanceOf(current),
		Other:   provenanceOf(other),
		Deltas:  Deltas(other.History(), current.History()),
	}

	otherByID := make(map[string]recordRow, len(other.PerRecord))
	for _, r := range other.PerRecord {
		otherByID[r.ID] = r
	}

	var counts [3][3]int
	seen := make(map[string]bool, len(current.PerRecord))
	for _, cur := range current.PerRecord {
		seen[cur.ID] = true
		oth, ok := otherByID[cur.ID]
		if !ok {
			c.OnlyInCurrent = append(c.OnlyInCurrent, cur.ID)
			continue
		}
		ci, cok := effectIndex(cur.Verdict)
		oi, ook := effectIndex(oth.Verdict)
		if !cok || !ook {
			continue
		}
		counts[ci][oi]++
		if cur.Verdict != oth.Verdict {
			c.Disagreements = append(c.Disagreements, Disagreement{
				ID:      cur.ID,
				Truth:   cur.Truth,
				Current: cur.Verdict,
				Other:   oth.Verdict,
			})
		}
		if !dangerousTruth(cur.Truth) {
			continue
		}
		switch {
		case cur.Verdict == string(core.Allow) && oth.Verdict != string(core.Allow):
			c.SafetyCritical.CurrentAllowsOtherFlags++
			c.SafetyCritical.Records = append(c.SafetyCritical.Records, SafetyCriticalRecord{
				ID:             cur.ID,
				Truth:          cur.Truth,
				AllowedBy:      current.Backend,
				FlaggedBy:      other.Backend,
				AllowedVerdict: cur.Verdict,
				FlaggedVerdict: oth.Verdict,
			})
		case oth.Verdict == string(core.Allow) && cur.Verdict != string(core.Allow):
			c.SafetyCritical.OtherAllowsCurrentFlags++
			c.SafetyCritical.Records = append(c.SafetyCritical.Records, SafetyCriticalRecord{
				ID:             cur.ID,
				Truth:          cur.Truth,
				AllowedBy:      other.Backend,
				FlaggedBy:      current.Backend,
				AllowedVerdict: oth.Verdict,
				FlaggedVerdict: cur.Verdict,
			})
		}
	}
	for _, r := range other.PerRecord {
		if !seen[r.ID] {
			c.OnlyInOther = append(c.OnlyInOther, r.ID)
		}
	}

	c.Matrix = Matrix{
		Allow: EffectCounts{Allow: counts[0][0], Ask: counts[0][1], Deny: counts[0][2]},
		Ask:   EffectCounts{Allow: counts[1][0], Ask: counts[1][1], Deny: counts[1][2]},
		Deny:  EffectCounts{Allow: counts[2][0], Ask: counts[2][1], Deny: counts[2][2]},
	}
	return c
}

// Write persists the comparison to dir as
// compare-<current-ts>-<current-backend>-vs-<other-backend>.json and returns
// the path (architecture §7). The directory is created when missing.
func (c Comparison) Write(dir string) (string, error) {
	name := fmt.Sprintf("compare-%s-%s-vs-%s.json",
		c.TS, safeName(c.Current.Backend), safeName(c.Other.Backend))
	path := filepath.Join(dir, name)
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// Table renders the comparison as a readable plain-text table for stdout. The
// layout is stable so contract tests can assert on its headings.
func (c Comparison) Table() string {
	s := fmt.Sprintf("against: %s (ts %s) vs current %s (ts %s)\n",
		c.Other.Backend, c.Other.TS, c.Current.Backend, c.Current.TS)
	s += "metrics (Δ = current − other):\n"
	for _, d := range c.Deltas {
		s += fmt.Sprintf("  %-12s other=%-10.4f current=%-10.4f Δ=%+.4f\n",
			d.Field, d.Previous, d.Current, d.Change)
	}
	s += fmt.Sprintf("agreement matrix (rows current=%s, cols other=%s):\n",
		c.Current.Backend, c.Other.Backend)
	s += "  current\\other   allow   ask    deny\n"
	for i := 0; i < 3; i++ {
		var row EffectCounts
		switch i {
		case 0:
			row = c.Matrix.Allow
		case 1:
			row = c.Matrix.Ask
		default:
			row = c.Matrix.Deny
		}
		s += fmt.Sprintf("  %-14s %6d  %5d  %6d\n", effectAt(i), row.Allow, row.Ask, row.Deny)
	}
	s += fmt.Sprintf("disagreements: %d\n", len(c.Disagreements))
	for _, d := range c.Disagreements {
		s += fmt.Sprintf("  id=%s truth=%s current=%s other=%s\n",
			d.ID, d.Truth, d.Current, d.Other)
	}
	s += fmt.Sprintf("safety-critical: current auto-allowed/other flagged = %d; other auto-allowed/current flagged = %d\n",
		c.SafetyCritical.CurrentAllowsOtherFlags, c.SafetyCritical.OtherAllowsCurrentFlags)
	for _, r := range c.SafetyCritical.Records {
		s += fmt.Sprintf("  id=%s truth=%s allowed_by=%s (%s) flagged_by=%s (%s)\n",
			r.ID, r.Truth, r.AllowedBy, r.AllowedVerdict, r.FlaggedBy, r.FlaggedVerdict)
	}
	return s
}
