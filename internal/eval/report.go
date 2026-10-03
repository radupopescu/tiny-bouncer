// Reports, history and gates (architecture §3 eval, §7): the report JSON,
// the one-line history append, gates checking, and the previous-run deltas
// behind `eval --compare`.
package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"wiseyolo/internal/backend"
)

// HistoryLine is one appended line of reports/history.jsonl (plan T09):
// exactly these fields, one per run.
type HistoryLine struct {
	TS                string  `json:"ts"`
	Backend           string  `json:"backend"`
	BackendModel      string  `json:"backend_model"`
	PolicyVersion     string  `json:"policy_version"`
	ThresholdsVersion string  `json:"thresholds_version"`
	Sensitivity       float64 `json:"sensitivity"`
	Specificity       float64 `json:"specificity"`
	Precision         float64 `json:"precision"`
	F1                float64 `json:"f1"`
	FNR               float64 `json:"fnr"`
	FPR               float64 `json:"fpr"`
	Accuracy3         float64 `json:"accuracy3"`
	LatP50            float64 `json:"lat_p50"`
	LatP95            float64 `json:"lat_p95"`
	CorpusSize        int     `json:"corpus_size"`
}

// safety is the derived safety-view rates of the confusion matrix.
type safety struct {
	Sensitivity      float64 `json:"sensitivity"`
	Specificity      float64 `json:"specificity"`
	Precision        float64 `json:"precision"`
	F1               float64 `json:"f1"`
	FPR              float64 `json:"fpr"`
	FNR              float64 `json:"fnr"`
	BalancedAccuracy float64 `json:"balanced_accuracy"`
}

// recordRow is one per-record table row of the report. Commands themselves
// appear in the corpus file only; the report carries the record id, truth and
// the verdict the backend produced.
type recordRow struct {
	ID         string   `json:"id"`
	Truth      string   `json:"truth"`
	Verdict    string   `json:"verdict"`
	Categories []string `json:"categories"`
	Reason     string   `json:"reason"`
	WallMS     int64    `json:"wall_ms"`
}

// Report is the full eval report written to reports/eval-<ts>-<backend>-<model>.json.
type Report struct {
	TS                string `json:"ts"`
	Backend           string `json:"backend"`
	BackendModel      string `json:"backend_model"`
	PolicyVersion     string `json:"policy_version"`
	ThresholdsVersion string `json:"thresholds_version"`
	CorpusSize        int    `json:"corpus_size"`

	TruePositive  int `json:"true_positive"`
	FalseNegative int `json:"false_negative"`
	FalsePositive int `json:"false_positive"`
	TrueNegative  int `json:"true_negative"`

	Safety    safety           `json:"safety"`
	ThreeWay  ThreeWay         `json:"three_way"`
	PerCat    []CategoryRecall `json:"per_category_recall"`
	Disguised SubsetFNRFPR     `json:"disguised"`
	Latency   Latency          `json:"latency"`
	// Usage is the summed token usage over the run (architecture §5bis);
	// zero for backends that do not report usage.
	Usage     Usage       `json:"usage"`
	Bench     *Bench      `json:"bench,omitempty"`
	PerRecord []recordRow `json:"per_record"`
}

// Usage is the token consumption of a full eval run.
type Usage struct {
	Requests     int `json:"requests"`
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// BuildReport computes the full report from a scored corpus. bench may be nil.
func BuildReport(ts time.Time, info backend.Info, set *Set, scored []Scored, bench *Bench, usage Usage) Report {
	c := SafetyConfusion(scored)
	rows := make([]recordRow, len(scored))
	lat := make([]int64, len(scored))
	for i, s := range scored {
		rows[i] = recordRow{
			ID:         s.Record.ID,
			Truth:      string(s.Record.Truth),
			Verdict:    string(s.Verdict),
			Categories: s.Categories,
			Reason:     s.Reason,
			WallMS:     s.WallMS,
		}
		lat[i] = s.WallMS
	}
	return Report{
		TS:                ts.UTC().Format(time.RFC3339),
		Backend:           info.Name,
		BackendModel:      info.Model,
		PolicyVersion:     info.PolicyVersion,
		ThresholdsVersion: info.ThresholdsVersion,
		CorpusSize:        len(scored),
		TruePositive:      c.TP,
		FalseNegative:     c.FN,
		FalsePositive:     c.FP,
		TrueNegative:      c.TN,
		Safety: safety{
			Sensitivity:      c.Sensitivity(),
			Specificity:      c.Specificity(),
			Precision:        c.Precision(),
			F1:               c.F1(),
			FPR:              c.FPR(),
			FNR:              c.FNR(),
			BalancedAccuracy: c.BalancedAccuracy(),
		},
		ThreeWay:  ThreeWayView(scored),
		PerCat:    PerCategoryRecall(scored, Categories),
		Disguised: DisguisedView(scored),
		Latency:   LatencyView(lat),
		Bench:     bench,
		PerRecord: rows,
	}
}

// History reduces the report to the one history-line summary.
func (rep Report) History() HistoryLine {
	return HistoryLine{
		TS:                rep.TS,
		Backend:           rep.Backend,
		BackendModel:      rep.BackendModel,
		PolicyVersion:     rep.PolicyVersion,
		ThresholdsVersion: rep.ThresholdsVersion,
		Sensitivity:       rep.Safety.Sensitivity,
		Specificity:       rep.Safety.Specificity,
		Precision:         rep.Safety.Precision,
		F1:                rep.Safety.F1,
		FNR:               rep.Safety.FNR,
		FPR:               rep.Safety.FPR,
		Accuracy3:         rep.ThreeWay.Accuracy,
		LatP50:            rep.Latency.P50,
		LatP95:            rep.Latency.P95,
		CorpusSize:        rep.CorpusSize,
	}
}

// safeName strips everything but letters, digits, dot and dash from a
// filename component.
func safeName(s string) string {
	return regexp.MustCompile(`[^A-Za-z0-9.-]+`).ReplaceAllString(s, "-")
}

// Write persists the report to dir as eval-<timestamp>-<backend>-<model>.json
// and returns the path. The directory is created when missing. An identical
// second-per-second rerun overwrites the same report; the history line still
// accumulates.
func (rep Report) Write(dir string) (string, error) {
	name := fmt.Sprintf("eval-%s-%s-%s.json",
		rep.TS, safeName(rep.Backend), safeName(rep.BackendModel))
	path := filepath.Join(dir, name)
	b, err := json.MarshalIndent(rep, "", "  ")
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

// AppendHistory adds one line to dir/history.jsonl, creating the file and the
// directory when they do not exist (architecture §7: one line per run).
func AppendHistory(dir string, h HistoryLine) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "history.jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// LoadHistory reads every history line; a missing file is an empty history.
func LoadHistory(dir string) ([]HistoryLine, error) {
	b, err := os.ReadFile(filepath.Join(dir, "history.jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lines []HistoryLine
	for i, raw := range splitLines(b) {
		if len(raw) == 0 {
			continue
		}
		var h HistoryLine
		if err := json.Unmarshal(raw, &h); err != nil {
			return nil, fmt.Errorf("history.jsonl line %d: %w", i+1, err)
		}
		lines = append(lines, h)
	}
	return lines, nil
}

func splitLines(b []byte) (out [][]byte) {
	start := 0
	for i, c := range b {
		if c == '\n' {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return
}

// LatestSameBackend returns the most recent history line recorded with the
// same backend id, if any.
func LatestSameBackend(lines []HistoryLine, backendName string) (HistoryLine, bool) {
	if lines == nil {
		return HistoryLine{}, false
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i].Backend == backendName {
			return lines[i], true
		}
	}
	return HistoryLine{}, false
}

// Delta is one before/after difference printed by `eval --compare`.
type Delta struct {
	Field    string  `json:"field"`
	Previous float64 `json:"previous"`
	Current  float64 `json:"current"`
	Change   float64 `json:"change"`
}

// Deltas computes the comparison fields between the previous and current run.
func Deltas(prev, cur HistoryLine) []Delta {
	pair := func(name string, p, c float64) Delta {
		return Delta{Field: name, Previous: p, Current: c, Change: c - p}
	}
	return []Delta{
		pair("sensitivity", prev.Sensitivity, cur.Sensitivity),
		pair("specificity", prev.Specificity, cur.Specificity),
		pair("precision", prev.Precision, cur.Precision),
		pair("f1", prev.F1, cur.F1),
		pair("fnr", prev.FNR, cur.FNR),
		pair("fpr", prev.FPR, cur.FPR),
		pair("accuracy3", prev.Accuracy3, cur.Accuracy3),
		pair("lat_p50", prev.LatP50, cur.LatP50),
		pair("lat_p95", prev.LatP95, cur.LatP95),
	}
}

// Gates are the regression gates applied by `eval --compare`
// (architecture §7; final values are set by task T10 from the first live
// run). Nil bounds are not enforced; a bounds file must be valid JSON of
// this shape, anything unknown is rejected by the strict loader.
type Gates struct {
	FNRMax      *float64 `json:"fnr_max"`
	FPRMax      *float64 `json:"fpr_max"`
	AccuracyMin *float64 `json:"accuracy3_min"`
	LatP95Max   *float64 `json:"lat_p95_ms_max"`
}

// LoadGates reads and validates a gates file.
func LoadGates(path string) (Gates, error) {
	var g Gates
	b, err := os.ReadFile(path)
	if err != nil {
		return g, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&g); err != nil {
		return g, fmt.Errorf("gates %s: %w", path, err)
	}
	for name, v := range map[string]*float64{
		"fnr_max": g.FNRMax, "fpr_max": g.FPRMax,
		"accuracy3_min": g.AccuracyMin, "lat_p95_ms_max": g.LatP95Max,
	} {
		if v != nil && *v < 0 {
			return g, fmt.Errorf("gates %s: value of %s is negative", path, name)
		}
	}
	return g, nil
}

// Violations returns one human line per gate the run violates, in gate order.
func (g Gates) Violations(h HistoryLine) []string {
	var out []string
	if g.FNRMax != nil && h.FNR > *g.FNRMax {
		out = append(out, fmt.Sprintf("FNR %.4g violates the maximum %.4g", h.FNR, *g.FNRMax))
	}
	if g.FPRMax != nil && h.FPR > *g.FPRMax {
		out = append(out, fmt.Sprintf("FPR %.4g violates the maximum %.4g", h.FPR, *g.FPRMax))
	}
	if g.AccuracyMin != nil && h.Accuracy3 < *g.AccuracyMin {
		out = append(out, fmt.Sprintf("three-way accuracy %.4g violates the minimum %.4g",
			h.Accuracy3, *g.AccuracyMin))
	}
	if g.LatP95Max != nil && h.LatP95 > *g.LatP95Max {
		out = append(out, fmt.Sprintf("p95 latency %.4g ms violates the maximum %.4g ms",
			h.LatP95, *g.LatP95Max))
	}
	return out
}
