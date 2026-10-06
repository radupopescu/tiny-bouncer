package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wiseyolo/internal/backend"
	"wiseyolo/internal/core"
)

// Hand-computed unit tests for the T15 cross-backend comparison. Every
// expectation is derived by hand from a small synthetic report pair, so a
// regression in the matrix, the disagreement list or the safety-critical tally
// is visible on failure.

// scoredRow is one synthetic per-record report row.
func scoredRow(id string, truth Truth, verdict core.Effect) Scored {
	return Scored{
		Record:  Record{ID: id, Truth: truth, Categories: []string{"disguised"}},
		Verdict: verdict,
	}
}

// reportFrom builds a full report from synthetic rows via the production
// builder, so Compare exercises real Report values rather than a hand-rolled
// struct.
func reportFrom(ts time.Time, name, model string, rows []Scored) Report {
	set := &Set{Records: make([]Record, len(rows))}
	for i, s := range rows {
		set.Records[i] = s.Record
	}
	return BuildReport(ts, backend.Info{
		Name: name, Model: model, PolicyVersion: "p1", ThresholdsVersion: "tv1",
	}, set, rows, nil, Usage{})
}

// comparisonFixture is the hand-computed pair used by the matrix test:
//
//	id  truth  current(api)  other(mock)
//	a1  deny   deny         deny
//	a2  deny   allow        deny    <- current auto-allows; other flags (critical)
//	a3  ask    ask          ask
//	a4  deny   deny         ask
//	a5  allow  allow        allow
//	a6  allow  ask          allow
//	a7  deny   deny         deny
//	a8  allow  allow        deny
//	a9  deny   deny         allow   <- other auto-allows; current flags (critical)
func comparisonFixture() (Report, Report) {
	current := reportFrom(time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC), "api", "gemma", []Scored{
		scoredRow("a1", TruthDeny, core.Deny),
		scoredRow("a2", TruthDeny, core.Allow),
		scoredRow("a3", TruthAsk, core.Ask),
		scoredRow("a4", TruthDeny, core.Deny),
		scoredRow("a5", TruthAllow, core.Allow),
		scoredRow("a6", TruthAllow, core.Ask),
		scoredRow("a7", TruthDeny, core.Deny),
		scoredRow("a8", TruthAllow, core.Allow),
		scoredRow("a9", TruthDeny, core.Deny),
	})
	other := reportFrom(time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC), "mock", "mock-rules", []Scored{
		scoredRow("a1", TruthDeny, core.Deny),
		scoredRow("a2", TruthDeny, core.Deny),
		scoredRow("a3", TruthAsk, core.Ask),
		scoredRow("a4", TruthDeny, core.Ask),
		scoredRow("a5", TruthAllow, core.Allow),
		scoredRow("a6", TruthAllow, core.Allow),
		scoredRow("a7", TruthDeny, core.Deny),
		scoredRow("a8", TruthAllow, core.Deny),
		scoredRow("a9", TruthDeny, core.Allow),
	})
	return current, other
}

func TestCompareMatrix(t *testing.T) {
	current, other := comparisonFixture()
	c := Compare(current, other)

	want := Matrix{
		Allow: EffectCounts{Allow: 1, Ask: 0, Deny: 2},
		Ask:   EffectCounts{Allow: 1, Ask: 1, Deny: 0},
		Deny:  EffectCounts{Allow: 1, Ask: 1, Deny: 2},
	}
	if c.Matrix != want {
		t.Errorf("matrix = %+v, want %+v", c.Matrix, want)
	}
	// The matrix must account for every id aligned on both sides.
	var total int
	for _, row := range []EffectCounts{c.Matrix.Allow, c.Matrix.Ask, c.Matrix.Deny} {
		total += row.Allow + row.Ask + row.Deny
	}
	if total != 9 {
		t.Errorf("matrix total = %d, want 9 aligned records", total)
	}

	if c.Current.Backend != "api" || c.Current.BackendModel != "gemma" ||
		c.Other.Backend != "mock" || c.Other.BackendModel != "mock-rules" {
		t.Errorf("provenance = %+v vs %+v", c.Current, c.Other)
	}
	if c.TS != current.TS {
		t.Errorf("comparison ts = %q, want current %q", c.TS, current.TS)
	}
	if len(c.Deltas) != 9 {
		t.Errorf("deltas = %d, want 9 fields", len(c.Deltas))
	}
}

func TestCompareDisagreements(t *testing.T) {
	current, other := comparisonFixture()
	c := Compare(current, other)

	want := []Disagreement{
		{ID: "a2", Truth: "deny", Current: "allow", Other: "deny"},
		{ID: "a4", Truth: "deny", Current: "deny", Other: "ask"},
		{ID: "a6", Truth: "allow", Current: "ask", Other: "allow"},
		{ID: "a8", Truth: "allow", Current: "allow", Other: "deny"},
		{ID: "a9", Truth: "deny", Current: "deny", Other: "allow"},
	}
	if len(c.Disagreements) != len(want) {
		t.Fatalf("disagreements = %+v, want %+v", c.Disagreements, want)
	}
	for i := range want {
		if c.Disagreements[i] != want[i] {
			t.Errorf("disagreement %d = %+v, want %+v", i, c.Disagreements[i], want[i])
		}
	}
}

func TestCompareSafetyCritical(t *testing.T) {
	current, other := comparisonFixture()
	c := Compare(current, other)

	sc := c.SafetyCritical
	if sc.CurrentAllowsOtherFlags != 1 || sc.OtherAllowsCurrentFlags != 1 {
		t.Errorf("safety-critical counts = %d/%d, want 1/1",
			sc.CurrentAllowsOtherFlags, sc.OtherAllowsCurrentFlags)
	}
	want := []SafetyCriticalRecord{
		{ID: "a2", Truth: "deny", AllowedBy: "api", FlaggedBy: "mock",
			AllowedVerdict: "allow", FlaggedVerdict: "deny"},
		{ID: "a9", Truth: "deny", AllowedBy: "mock", FlaggedBy: "api",
			AllowedVerdict: "allow", FlaggedVerdict: "deny"},
	}
	if len(sc.Records) != len(want) {
		t.Fatalf("safety-critical records = %+v, want %+v", sc.Records, want)
	}
	for i := range want {
		if sc.Records[i] != want[i] {
			t.Errorf("safety-critical record %d = %+v, want %+v", i, sc.Records[i], want[i])
		}
	}
}

// TestCompareCoverage checks that ids present in only one report are listed
// but excluded from the matrix, and that a both-wrong (both allow a dangerous
// truth) pair is not counted as safety-critical — neither side flagged it.
func TestCompareCoverage(t *testing.T) {
	current := reportFrom(time.Now(), "api", "gemma", []Scored{
		scoredRow("a1", TruthDeny, core.Allow), // both allow dangerous: not safety-critical
		scoredRow("only-current", TruthAllow, core.Allow),
	})
	other := reportFrom(time.Now(), "mock", "mock-rules", []Scored{
		scoredRow("a1", TruthDeny, core.Allow),
		scoredRow("only-other", TruthAllow, core.Allow),
	})
	c := Compare(current, other)
	if len(c.OnlyInCurrent) != 1 || c.OnlyInCurrent[0] != "only-current" {
		t.Errorf("only-in-current = %v", c.OnlyInCurrent)
	}
	if len(c.OnlyInOther) != 1 || c.OnlyInOther[0] != "only-other" {
		t.Errorf("only-in-other = %v", c.OnlyInOther)
	}
	total := c.Matrix.Allow.Allow + c.Matrix.Allow.Ask + c.Matrix.Allow.Deny +
		c.Matrix.Ask.Allow + c.Matrix.Ask.Ask + c.Matrix.Ask.Deny +
		c.Matrix.Deny.Allow + c.Matrix.Deny.Ask + c.Matrix.Deny.Deny
	if total != 1 {
		t.Errorf("matrix total = %d, want 1 aligned record", total)
	}
	if sc := c.SafetyCritical; sc.CurrentAllowsOtherFlags != 0 || sc.OtherAllowsCurrentFlags != 0 ||
		len(sc.Records) != 0 {
		t.Errorf("both-allowed dangerous truth counted as safety-critical: %+v", sc)
	}
}

func TestLatestReportSelectsNewest(t *testing.T) {
	dir := t.TempDir()
	mk := func(ts, name string, verdict core.Effect) string {
		rep := reportFrom(time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC), name, "m", []Scored{
			scoredRow("r1", TruthDeny, verdict),
		})
		rep.TS = ts
		path, err := rep.Write(dir)
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
	mk("2026-10-06T09:00:00Z", "mock", core.Deny)
	mk("2026-10-06T11:00:00Z", "mock", core.Ask)  // newest mock
	mk("2026-10-06T10:00:00Z", "api", core.Allow) // different backend

	rep, found, err := LatestReport(dir, "mock")
	if err != nil || !found {
		t.Fatalf("LatestReport(mock) = %v found=%v err=%v", rep, found, err)
	}
	if rep.TS != "2026-10-06T11:00:00Z" {
		t.Errorf("latest mock ts = %q, want the newest", rep.TS)
	}
	if len(rep.PerRecord) != 1 || rep.PerRecord[0].Verdict != "ask" {
		t.Errorf("latest report rows = %+v", rep.PerRecord)
	}

	if _, found, err := LatestReport(dir, "afm"); err != nil || found {
		t.Errorf("absent backend: found=%v err=%v, want found=false", found, err)
	}
	// A missing directory is not an error either.
	if _, found, err := LatestReport(filepath.Join(dir, "nope"), "mock"); err != nil || found {
		t.Errorf("missing dir: found=%v err=%v, want found=false", found, err)
	}
}

func TestComparisonWriteAndTable(t *testing.T) {
	current, other := comparisonFixture()
	c := Compare(current, other)
	dir := t.TempDir()
	path, err := c.Write(dir)
	if err != nil {
		t.Fatalf("write compare: %v", err)
	}
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "compare-"+current.TS+"-api-vs-mock.json") {
		t.Errorf("compare report name = %q", base)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var back Comparison
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("compare report JSON: %v\n%s", err, b)
	}
	if back.Other.Backend != "mock" || back.SafetyCritical.CurrentAllowsOtherFlags != 1 {
		t.Errorf("round-tripped comparison = %+v", back)
	}

	table := c.Table()
	for _, want := range []string{"agreement matrix", "disagreements: 5", "safety-critical",
		"id=a2", "allowed_by=api", "other="} {
		if !strings.Contains(table, want) {
			t.Errorf("table lacks %q:\n%s", want, table)
		}
	}
}
