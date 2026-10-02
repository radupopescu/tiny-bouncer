package main_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wiseyolo/internal/eval"
)

// This file holds the subprocess contract tests for the eval subcommand
// (architecture §3 eval, §7 metrics/gates; plan T09 acceptance criteria).
// Every run uses the mock backend and a temporary reports directory, so no
// test touches the network or the tracked reports/history.jsonl.

// corpusPath is the evalset location relative to the package directory.
const corpusPath = "../../data/evalset.json"

// evalReport is the subset of the report JSON the contract tests assert on.
type evalReport struct {
	Backend       string `json:"backend"`
	BackendModel  string `json:"backend_model"`
	CorpusSize    int    `json:"corpus_size"`
	TruePositive  int    `json:"true_positive"`
	FalseNegative int    `json:"false_negative"`
	FalsePositive int    `json:"false_positive"`
	TrueNegative  int    `json:"true_negative"`
	Safety        struct {
		Sensitivity      float64 `json:"sensitivity"`
		Specificity      float64 `json:"specificity"`
		Precision        float64 `json:"precision"`
		F1               float64 `json:"f1"`
		FNR              float64 `json:"fnr"`
		FPR              float64 `json:"fpr"`
		BalancedAccuracy float64 `json:"balanced_accuracy"`
	} `json:"safety"`
	ThreeWay struct {
		Accuracy float64 `json:"accuracy"`
	} `json:"three_way"`
	Disguised struct {
		FNR float64 `json:"fnr"`
		FPR float64 `json:"fpr"`
	} `json:"disguised"`
	Latency struct {
		P50 float64 `json:"p50"`
		P95 float64 `json:"p95"`
	} `json:"latency"`
	Bench *struct {
		Runs   int     `json:"runs"`
		MeanMS float64 `json:"mean_ms"`
		P95MS  float64 `json:"p95_ms"`
	} `json:"bench"`
	PerRecord []struct {
		ID      string `json:"id"`
		Truth   string `json:"truth"`
		Verdict string `json:"verdict"`
		WallMS  int64  `json:"wall_ms"`
	} `json:"per_record"`
}

// historySubset is the parsed subset of one history line.
type historySubset struct {
	Backend     string  `json:"backend"`
	Sensitivity float64 `json:"sensitivity"`
	Specificity float64 `json:"specificity"`
	Precision   float64 `json:"precision"`
	F1          float64 `json:"f1"`
	FNR         float64 `json:"fnr"`
	FPR         float64 `json:"fpr"`
	Accuracy3   float64 `json:"accuracy3"`
	CorpusSize  int     `json:"corpus_size"`
}

// readHistory parses every line of dir/history.jsonl.
func readHistory(t *testing.T, dir string) []historySubset {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "history.jsonl"))
	if err != nil {
		t.Fatalf("history.jsonl: %v", err)
	}
	var out []historySubset
	for i, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		var h historySubset
		if err := json.Unmarshal([]byte(line), &h); err != nil {
			t.Fatalf("history line %d not JSON: %v\n%s", i+1, err, line)
		}
		out = append(out, h)
	}
	return out
}

func TestEvalBinaryMockEndToEndAndDeterminism(t *testing.T) {
	dir := t.TempDir()
	corpus, err := eval.LoadEvalSet(corpusPath)
	if err != nil {
		t.Fatalf("corpus: %v", err)
	}

	// First run: exit 0, report written, history line appended.
	code, stdout, stderr := runBinary(t, checkBin, "", nil,
		"eval", "--backend", "mock", "--corpus", corpusPath, "--reports", dir)
	if code != 0 || stderr != "" {
		t.Fatalf("first run: exit=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "report: ") {
		t.Fatalf("first run stdout lacks the report path: %q", stdout)
	}
	// The report file exists (gitignored, eval-*.json).
	entries, err := filepath.Glob(filepath.Join(dir, "eval-*.json"))
	if err != nil || len(entries) != 1 || !strings.Contains(filepath.Base(entries[0]), "-mock-mock-rules.json") {
		t.Fatalf("report files = %v err=%v, want one eval-<ts>-mock-mock-rules.json", entries, err)
	}
	b, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	var rep evalReport
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatalf("report JSON: %v\n%s", err, b)
	}
	if rep.Backend != "mock" || rep.BackendModel != "mock-rules" {
		t.Errorf("report identity = %s/%s", rep.Backend, rep.BackendModel)
	}
	if rep.CorpusSize != len(corpus.Records) {
		t.Errorf("corpus_size = %d, want %d", rep.CorpusSize, len(corpus.Records))
	}
	if len(rep.PerRecord) != rep.CorpusSize {
		t.Errorf("per-record rows = %d, want %d", len(rep.PerRecord), rep.CorpusSize)
	}
	if rep.TruePositive+rep.FalseNegative+rep.FalsePositive+rep.TrueNegative != rep.CorpusSize {
		t.Errorf("confusion does not sum to corpus size: %+v", rep)
	}
	if rep.Latency.P50 < 0 || rep.Latency.P95 < rep.Latency.P50 {
		t.Errorf("latency odd: %+v", rep.Latency)
	}

	h1 := readHistory(t, dir)
	if len(h1) != 1 {
		t.Fatalf("after one run, history = %d lines, want 1", len(h1))
	}
	if h1[0].Backend != "mock" || h1[0].CorpusSize != rep.CorpusSize ||
		h1[0].Sensitivity != rep.Safety.Sensitivity || h1[0].Accuracy3 != rep.ThreeWay.Accuracy {
		t.Errorf("history line does not match the report: %+v vs %+v", h1[0], rep)
	}

	// Second run appends a second line and is deterministic in every metric.
	_, _, _ = runBinary(t, checkBin, "", nil,
		"eval", "--backend", "mock", "--corpus", corpusPath, "--reports", dir)
	h2 := readHistory(t, dir)
	if len(h2) != 2 {
		t.Fatalf("after two runs, history = %d lines, want 2", len(h2))
	}
	a, c := h1[0], h2[1]
	if a.Sensitivity != c.Sensitivity || a.Specificity != c.Specificity ||
		a.Precision != c.Precision || a.F1 != c.F1 || a.FNR != c.FNR ||
		a.FPR != c.FPR || a.Accuracy3 != c.Accuracy3 || a.CorpusSize != c.CorpusSize {
		t.Fatalf("two mock runs differ: %+v vs %+v", a, c)
	}
}

func TestEvalBinaryComparePrintsDeltasAndGateNote(t *testing.T) {
	dir := t.TempDir()
	// Only one run of history exists here; --compare must still exit 0 and
	// print the gates-unset note.
	_, _, _ = runBinary(t, checkBin, "", nil,
		"eval", "--backend", "mock", "--corpus", corpusPath, "--reports", dir)
	code, stdout, stderr := runBinary(t, checkBin, "", nil,
		"eval", "--backend", "mock", "--corpus", corpusPath, "--reports", dir, "--compare")
	if code != 0 {
		t.Fatalf("compare exit = %d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "compare:") {
		t.Errorf("stdout lacks delta output: %q", stdout)
	}
	if !strings.Contains(stdout, "gates: ") {
		t.Errorf("stdout lacks the gates note: %q", stdout)
	}
}

// TestEvalBinaryGatesContract covers --gates both directions without network:
// a passing file exits 0, a violating synthetic file exits non-zero (3) with
// the violation on stderr.
func TestEvalBinaryGatesContract(t *testing.T) {
	dir := t.TempDir()
	passing := filepath.Join(dir, "pass.json")
	violating := filepath.Join(dir, "fail.json")
	for name, body := range map[string]string{
		passing:   `{"fnr_max": 1.0, "fpr_max": 1.0, "accuracy3_min": 0.0, "lat_p95_ms_max": 60000}`,
		violating: `{"accuracy3_min": 2.0}`,
	} {
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	args := func(gates string) []string {
		return []string{"eval", "--backend", "mock", "--corpus", corpusPath,
			"--reports", dir, "--gates", gates}
	}

	code, stdout, stderr := runBinary(t, checkBin, "", nil, args(passing)...)
	if code != 0 || !strings.Contains(stdout, "gates: PASS") {
		t.Fatalf("passing gates: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	code, stdout, stderr = runBinary(t, checkBin, "", nil, args(violating)...)
	if code != 3 {
		t.Fatalf("violating gates: exit=%d, want 3 (gates violation)", code)
	}
	if !strings.Contains(stderr, "gates:") || !strings.Contains(stderr, "violates") {
		t.Fatalf("violating gates stderr = %q, want a violation line", stderr)
	}

	// A gates file that does not exist is a config error.
	code, _, stderr = runBinary(t, checkBin, "", nil, args(filepath.Join(dir, "absent.json"))...)
	if code != 1 || stderr == "" {
		t.Fatalf("absent explicit gates file: exit=%d stderr=%q, want exit 1", code, stderr)
	}
}

func TestEvalBinarySweepRequiresJev(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := runBinary(t, checkBin, "", nil,
		"eval", "--backend", "mock", "--corpus", corpusPath,
		"--reports", dir, "--sweep", "deny_hazard=0.9")
	if code != 1 {
		t.Fatalf("sweep on mock: exit=%d, want 1", code)
	}
	if !strings.Contains(stderr, "no threshold override support") {
		t.Fatalf("stderr = %q, want the override-support error", stderr)
	}
	if strings.Contains(stdout, "sweep") {
		t.Errorf("no sweep table may be printed for an unsupported backend")
	}
}

func TestEvalBinaryBenchSpawn(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := runBinary(t, checkBin, "", nil,
		"eval", "--backend", "mock", "--corpus", corpusPath,
		"--reports", dir, "--bench-spawn")
	if code != 0 {
		t.Fatalf("bench-spawn: exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "spawn bench: runs=") {
		t.Fatalf("stdout lacks the spawn bench summary: %q", stdout)
	}
	entries, _ := filepath.Glob(filepath.Join(dir, "eval-*.json"))
	if len(entries) != 1 {
		t.Fatalf("report files = %v", entries)
	}
	b, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	var rep evalReport
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatalf("report JSON: %v", err)
	}
	if rep.Bench == nil || rep.Bench.Runs < 20 {
		t.Fatalf("bench section = %+v, want runs >= 20", rep.Bench)
	}
	if rep.Bench.MeanMS <= 0 {
		t.Errorf("bench numbers odd: runs=%d mean=%.2f p95=%.2f", rep.Bench.Runs, rep.Bench.MeanMS, rep.Bench.P95MS)
	}
}

func TestEvalBinaryUsageErrors(t *testing.T) {
	// Unknown backend and unknown flags exit 1 with diagnostics on stderr.
	code, _, stderr := runBinary(t, checkBin, "", nil, "eval", "--backend", "nope")
	if code != 1 || !strings.Contains(stderr, "unknown backend") {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	code, _, stderr = runBinary(t, checkBin, "", nil, "eval", "--no-such-flag")
	if code != 1 || stderr == "" {
		t.Fatalf("unknown flag: exit=%d stderr=%q", code, stderr)
	}
	code, _, stderr = runBinary(t, checkBin, "", nil, "eval", "--backend", "mock", "--", "stray")
	if code != 1 || !strings.Contains(stderr, "unexpected argument") {
		t.Fatalf("stray argument: exit=%d stderr=%q", code, stderr)
	}
}
