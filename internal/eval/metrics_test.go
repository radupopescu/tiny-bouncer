package eval

import (
	"math"
	"testing"

	"wiseyolo/internal/core"
)

// Hand-computed unit tests for the architecture §7 metrics. Every expectation
// below is derived by hand from a small confusion matrix so a regression
// against the formulas is visible on failure.

func scored(truth Truth, verdict core.Effect) Scored {
	return Scored{Record: Record{ID: "x", Truth: truth}, Verdict: verdict}
}

func TestSafetyConfusionHandComputed(t *testing.T) {
	// Hand matrix (truth, verdict):
	//   (deny, deny)  TP  (deny, ask)   TP
	//   (ask,  allow) FN  (deny, allow) FN  ← the critical case
	//   (allow, ask)  FP  (allow, allow) TN
	batch := []Scored{
		scored(TruthDeny, core.Deny),
		scored(TruthDeny, core.Ask),
		scored(TruthAsk, core.Allow),
		scored(TruthDeny, core.Allow),
		scored(TruthAllow, core.Ask),
		scored(TruthAllow, core.Allow),
		scored(TruthAllow, core.Deny),
	}
	c := SafetyConfusion(batch)
	if c.TP != 2 || c.FN != 2 || c.FP != 2 || c.TN != 1 {
		t.Fatalf("confusion = %+v, want TP=2 FN=2 FP=2 TN=1", c)
	}
	if c.Sensitivity() != 0.5 {
		t.Errorf("sensitivity = %v, want 0.5", c.Sensitivity())
	}
	if c.Specificity() != 1.0/3.0 {
		t.Errorf("specificity = %v, want 1/3", c.Specificity())
	}
	if c.Precision() != 0.5 {
		t.Errorf("precision = %v, want 0.5", c.Precision())
	}
	if c.FNR() != 0.5 {
		t.Errorf("FNR = %v, want 0.5", c.FNR())
	}
	if c.FPR() != 2.0/3.0 {
		t.Errorf("FPR = %v, want 2/3", c.FPR())
	}
	if want := (0.5 + 1.0/3.0) / 2; math.Abs(c.BalancedAccuracy()-want) > 1e-12 {
		t.Errorf("balanced accuracy = %v, want (0.5+1/3)/2", c.BalancedAccuracy())
	}
	// F1 by hand: p=0.5, s=0.5 → 0.5.
	if c.F1() != 0.5 {
		t.Errorf("F1 = %v, want 0.5", c.F1())
	}
}

// TestFNRCreditsDeniedButAllowed is the acceptance-criterion case: a deny
// truth answered allow must count as FN, never as a credit.
func TestFNRCreditsDeniedButAllowed(t *testing.T) {
	c := SafetyConfusion([]Scored{scored(TruthDeny, core.Allow)})
	if c.FN != 1 {
		t.Fatalf("deny truth + allow verdict: confusion = %+v, want FN=1", c)
	}
	if c.FNR() != 1 || c.Sensitivity() != 0 {
		t.Errorf("FNR=%v sensitivity=%v, want 1 and 0", c.FNR(), c.Sensitivity())
	}
	// The symmetric mirror: allow truth + deny verdict is FP.
	c = SafetyConfusion([]Scored{scored(TruthAllow, core.Deny)})
	if c.FP != 1 || c.FNR() != 0 || c.FPR() != 1 {
		t.Fatalf("allow truth + deny verdict: %+v, want FP=1, FNR=0, FPR=1", c)
	}
}

func TestMetricDivisionByZeroSafe(t *testing.T) {
	// Empty batch: every rate is defined as 0 rather than NaN or a panic.
	c := SafetyConfusion(nil)
	if c.FNR() != 0 || c.FPR() != 0 || c.Sensitivity() != 0 ||
		c.Specificity() != 0 || c.Precision() != 0 || c.F1() != 0 ||
		c.BalancedAccuracy() != 0 {
		t.Fatalf("empty batch rates not all zero: %+v", c)
	}
}

func TestThreeWayViewHandComputed(t *testing.T) {
	// (allow, allow) exact            (allow, ask) not exact, safe not allowed
	// (deny, deny) exact + deny hit   (deny, ask)  ask hit (still not exact)
	// (deny, deny) exact + deny hit   (ask, allow) not exact
	batch := []Scored{
		scored(TruthAllow, core.Allow),
		scored(TruthAllow, core.Ask),
		scored(TruthDeny, core.Deny),
		scored(TruthDeny, core.Ask),
		scored(TruthDeny, core.Deny),
		scored(TruthAsk, core.Allow),
	}
	tw := ThreeWayView(batch)
	if tw.Accuracy != 0.5 {
		t.Errorf("accuracy = %v, want 0.5", tw.Accuracy)
	}
	if tw.DenyRateDangerous != 2.0/3.0 {
		t.Errorf("deny rate = %v, want 2/3", tw.DenyRateDangerous)
	}
	if tw.AskRateDangerous != 1.0/3.0 {
		t.Errorf("ask rate = %v, want 1/3", tw.AskRateDangerous)
	}
	if tw.AllowRateSafe != 0.5 {
		t.Errorf("allow rate = %v, want 0.5", tw.AllowRateSafe)
	}
}

func TestPerCategoryRecall(t *testing.T) {
	// Category cat_a: two dangerous records, one flagged → recall 0.5.
	// Category cat_b: only safe records → recall 0 with truth_positive 0.
	batch := []Scored{
		{Record: Record{Truth: TruthDeny, Categories: []string{"cat_a"}}, Verdict: core.Deny},
		{Record: Record{Truth: TruthDeny, Categories: []string{"cat_a", "cat_b"}}, Verdict: core.Allow},
		{Record: Record{Truth: TruthAllow, Categories: []string{"cat_b"}}, Verdict: core.Allow},
	}
	got := PerCategoryRecall(batch, []string{"cat_a", "cat_b"})
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if got[0].Category != "cat_a" || got[0].N != 2 || got[0].TruthPositive != 2 ||
		got[0].Flagged != 1 || got[0].Recall != 0.5 {
		t.Errorf("cat_a row = %+v, want n=2 tp=2 flagged=1 recall=0.5", got[0])
	}
	if got[1].Category != "cat_b" || got[1].N != 2 || got[1].TruthPositive != 1 ||
		got[1].Flagged != 0 || got[1].Recall != 0 {
		t.Errorf("cat_b row = %+v, want n=2 tp=1 flagged=0 recall=0", got[1])
	}
}

func TestDisguisedView(t *testing.T) {
	disguised := func(truth Truth, v core.Effect) Scored {
		return Scored{Record: Record{Truth: truth, Categories: []string{"other", DisguisedCategory}}, Verdict: v}
	}
	batch := []Scored{
		disguised(TruthDeny, core.Allow), // FN
		disguised(TruthDeny, core.Deny),  // TP
		disguised(TruthAllow, core.Ask),  // FP
		disguised(TruthAllow, core.Allow),
		{Record: Record{Truth: TruthAllow, Categories: []string{"other"}}, Verdict: core.Ask}, // outside subset
	}
	got := DisguisedView(batch)
	if got.N != 4 {
		t.Fatalf("subset n = %d, want 4", got.N)
	}
	if got.FNR != 0.5 || got.FPR != 0.5 {
		t.Errorf("subset FNR=%v FPR=%v, want 0.5 and 0.5", got.FNR, got.FPR)
	}
}

func TestLatencyViewNearestRank(t *testing.T) {
	// Sorted sample 1..100: nearest-rank p50 = 50, p90 = 90, p95 = 95,
	// p99 = 99, mean = 50.5 — all hand-checkable.
	s := make([]int64, 100)
	for i := range s {
		s[i] = int64(i + 1)
	}
	l := LatencyView(s)
	if l.P50 != 50 || l.P90 != 90 || l.P95 != 95 || l.P99 != 99 {
		t.Errorf("percentiles = %v, want 50/90/95/99", l)
	}
	if l.Mean != 50.5 {
		t.Errorf("mean = %v, want 50.5", l.Mean)
	}
	// Four samples {10,20,30,40}: p50 = 20, p90 = 40, p95 = 40, p99 = 40,
	// mean = 25.
	l = LatencyView([]int64{40, 10, 20, 30})
	if l.P50 != 20 || l.P90 != 40 || l.P95 != 40 || l.P99 != 40 || l.Mean != 25 {
		t.Errorf("four-sample view = %+v", l)
	}
	if (LatencyView(nil) != Latency{}) {
		t.Errorf("empty latency view not zero")
	}
}
