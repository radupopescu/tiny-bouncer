package eval

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wiseyolo/internal/backend"
)

// TestReportWriteAndAppendHistory drives the report file naming, the
// history.jsonl append, and the reload path end to end on a temp dir.
func TestReportWriteAndAppendHistory(t *testing.T) {
	dir := t.TempDir()
	set := &Set{Records: []Record{
		{ID: "r1", Truth: TruthDeny, Categories: []string{"sudo"}},
		{ID: "r2", Truth: TruthAllow, Categories: []string{"fs_read"}},
	}}
	scored := []Scored{
		{Record: set.Records[0], Verdict: "deny", Reason: "rule", WallMS: 3},
		{Record: set.Records[1], Verdict: "allow", Reason: "rule", WallMS: 11},
	}
	rep := BuildReport(time.Now(), backend.Info{
		Name: "mock", Model: "mock-rules", PolicyVersion: "mock-0", ThresholdsVersion: "mock-0",
	}, set, scored, nil, Usage{})

	path, err := rep.Write(dir)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "eval-") || !strings.Contains(base, "-mock-mock-rules.json") {
		t.Errorf("report name %q, want eval-<ts>-mock-mock-rules.json", base)
	}
	// The report must parse back and carry the metrics and rows.
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"sensitivity"`) || !strings.Contains(string(b), `"per_record"`) {
		t.Errorf("report JSON missing expected sections:\n%s", b)
	}

	if len(rep.PerRecord) != 2 {
		t.Fatalf("per-record rows = %d, want 2", len(rep.PerRecord))
	}
	if rep.PerRecord[0].Verdict != "deny" || rep.PerRecord[1].WallMS != 11 {
		t.Errorf("per-record rows = %+v", rep.PerRecord)
	}
	if rep.FalseNegative != 0 || rep.TruePositive != 1 || rep.TrueNegative != 1 {
		t.Errorf("confusion = %d/%d/%d/%d, want 1/0/0/1",
			rep.TruePositive, rep.FalseNegative, rep.FalsePositive, rep.TrueNegative)
	}
	if rep.ThreeWay.Accuracy != 1 || rep.Disguised.N != 0 {
		t.Errorf("three-way = %+v disguised = %+v", rep.ThreeWay, rep.Disguised)
	}

	if err := AppendHistory(dir, rep.History()); err != nil {
		t.Fatalf("append history: %v", err)
	}
	lines, err := LoadHistory(dir)
	if err != nil || len(lines) != 1 {
		t.Fatalf("history = %v %d lines, want 1 line and no error", err, len(lines))
	}
	if lines[0].Backend != "mock" || lines[0].CorpusSize != 2 ||
		lines[0].FNR != 0 || lines[0].Accuracy3 != 1 {
		t.Errorf("history line = %+v", lines[0])
	}

	// Missing history file reads as empty history.
	empty, err := LoadHistory(t.TempDir())
	if err != nil || empty != nil {
		t.Errorf("missing history: %v %v", empty, err)
	}
}

// TestGatesAndDeltas covers the gate arithmetic and the --compare delta list
// against hand values.
func TestGatesAndDeltas(t *testing.T) {
	passing := `{"fnr_max": 0, "fpr_max": 0.15, "accuracy3_min": 0.80, "lat_p95_ms_max": 1200}`
	dir := t.TempDir()
	gp := filepath.Join(dir, "gates.json")
	if err := os.WriteFile(gp, []byte(passing), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := LoadGates(gp)
	if err != nil {
		t.Fatalf("load gates: %v", err)
	}
	if v := g.Violations(HistoryLine{FNR: 0, FPR: 0.14, Accuracy3: 0.81, LatP95: 100}); len(v) != 0 {
		t.Errorf("clean line reported violations: %v", v)
	}
	if v := g.Violations(HistoryLine{FNR: 0.01, FPR: 0.16, Accuracy3: 0.79, LatP95: 1300}); len(v) != 4 {
		t.Errorf("violating line: %v", v)
	}
	if _, err := LoadGates(filepath.Join(dir, "absent.json")); err == nil {
		t.Errorf("absent gates file must error")
	}
	if err := os.WriteFile(gp, []byte(`{"bogus": 1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGates(gp); err == nil {
		t.Errorf("unknown gates key must be rejected")
	}
	if err := os.WriteFile(gp, []byte(`{"fnr_max": -1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGates(gp); err == nil {
		t.Errorf("negative gate bound must be rejected")
	}

	prev := HistoryLine{Sensitivity: 0.9, FPR: 0.1, LatP95: 100}
	cur := HistoryLine{Sensitivity: 0.8, FPR: 0.2, LatP95: 150}
	d := Deltas(prev, cur)
	if len(d) != 9 || d[0].Field != "sensitivity" || math.Abs(d[0].Change-(-0.1)) > 1e-12 ||
		d[8].Field != "lat_p95" || math.Abs(d[8].Change-50) > 1e-12 {
		t.Errorf("deltas = %+v", d)
	}
}

func TestLatestSameBackend(t *testing.T) {
	lines := []HistoryLine{
		{TS: "t1", Backend: "mock"},
		{TS: "t2", Backend: "jev"},
		{TS: "t3", Backend: "mock"},
	}
	if h, ok := LatestSameBackend(lines, "mock"); !ok || h.TS != "t3" {
		t.Errorf("latest mock = %+v ok=%v, want t3", h, ok)
	}
	if _, ok := LatestSameBackend(lines, "none"); ok {
		t.Errorf("unknown backend must not match")
	}
	if _, ok := LatestSameBackend(nil, "mock"); ok {
		t.Errorf("nil history must not match")
	}
}
