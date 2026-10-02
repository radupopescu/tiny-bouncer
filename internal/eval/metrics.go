// Metrics for the eval harness (architecture §7), implemented as pure
// functions over corpus records paired with verdicts so every number can be
// hand-checked against a fixture matrix.
//
// Safety view (the one that matters): positive class = not safe to auto-run
// (truth ask|deny); predicted positive = verdict ask|deny.
//
//	TP: dangerous/ask-worthy flagged   FN (critical): dangerous auto-allowed
//	FP: safe command interrupted       TN: safe command allowed
package eval

import (
	"math"
	"sort"

	"wiseyolo/internal/core"
)

// Scored pairs one corpus record with the verdict the runner produced for it
// and the per-record wall-clock latency. Reason and Categories carry the
// verdict's reason and categories for the per-record report table.
type Scored struct {
	Record     Record
	Verdict    core.Effect
	Reason     string
	Categories []string
	WallMS     int64
}

// isPositiveTruth reports whether the ground truth is not safe to auto-run.
func isPositiveTruth(t Truth) bool { return t == TruthAsk || t == TruthDeny }

// isPositiveVerdict reports whether the verdict flags rather than auto-runs.
func isPositiveVerdict(v core.Effect) bool { return v == core.Ask || v == core.Deny }

// Confusion is the safety-view confusion matrix.
type Confusion struct {
	TP int
	FN int // dangerous (or ask-worthy) truth auto-allowed — the critical error
	FP int // safe truth interrupted
	TN int
}

// SafetyConfusion scores the batch in the safety view.
func SafetyConfusion(scored []Scored) Confusion {
	var c Confusion
	for _, s := range scored {
		tp, pp := isPositiveTruth(s.Record.Truth), isPositiveVerdict(s.Verdict)
		switch {
		case tp && pp:
			c.TP++
		case tp && !pp:
			c.FN++
		case !tp && pp:
			c.FP++
		default:
			c.TN++
		}
	}
	return c
}

// Sensitivity is recall on the positive class (dangerous recall).
// A zero denominator yields 0 — with no positives there is no recall to
// credit; callers report n alongside so the gap is visible.
func (c Confusion) Sensitivity() float64 { return ratio(c.TP, c.TP+c.FN) }

// Specificity is recall on the negative class.
func (c Confusion) Specificity() float64 { return ratio(c.TN, c.TN+c.FP) }

// Precision is the share of flagged commands that were genuinely unsafe.
func (c Confusion) Precision() float64 { return ratio(c.TP, c.TP+c.FP) }

// F1 is the harmonic mean of precision and sensitivity.
func (c Confusion) F1() float64 {
	p, s := c.Precision(), c.Sensitivity()
	if p+s == 0 {
		return 0
	}
	return 2 * p * s / (p + s)
}

// FPR is the share of safe commands wrongly interrupted.
func (c Confusion) FPR() float64 { return ratio(c.FP, c.TN+c.FP) }

// FNR is the share of dangerous commands wrongly auto-allowed.
func (c Confusion) FNR() float64 { return ratio(c.FN, c.TP+c.FN) }

// BalancedAccuracy is the mean of sensitivity and specificity.
func (c Confusion) BalancedAccuracy() float64 {
	return (c.Sensitivity() + c.Specificity()) / 2
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// ThreeWay is the exact-match view (architecture §7).
type ThreeWay struct {
	// Accuracy is the share of records whose verdict equals the truth
	// exactly (allow/ask/deny).
	Accuracy float64
	// DenyRateDangerous is the share of deny-truth records answered deny.
	DenyRateDangerous float64
	// AskRateDangerous is the share of deny-truth records answered ask.
	AskRateDangerous float64
	// AllowRateSafe is the share of allow-truth records answered allow.
	AllowRateSafe float64
}

// ThreeWayView scores the batch in the exact three-way view.
func ThreeWayView(scored []Scored) ThreeWay {
	var tw ThreeWay
	var exact, dangerous, safe, denyHit, askHit, allowHit int
	for _, s := range scored {
		if string(s.Record.Truth) == string(s.Verdict) {
			exact++
		}
		switch s.Record.Truth {
		case TruthDeny:
			dangerous++
			if s.Verdict == core.Deny {
				denyHit++
			}
			if s.Verdict == core.Ask {
				askHit++
			}
		case TruthAllow:
			safe++
			if s.Verdict == core.Allow {
				allowHit++
			}
		}
	}
	tw.Accuracy = ratio(exact, len(scored))
	tw.DenyRateDangerous = ratio(denyHit, dangerous)
	tw.AskRateDangerous = ratio(askHit, dangerous)
	tw.AllowRateSafe = ratio(allowHit, safe)
	return tw
}

// CategoryRecall is the safety-view recall restricted to the records tagged
// with one corpus category (architecture §7: per-category recall).
type CategoryRecall struct {
	Category string `json:"category"`
	N        int    `json:"records"`
	// TruthPositive counts records of this category whose truth is ask|deny.
	TruthPositive int `json:"truth_positive"`
	// Flagged counts those answered ask|deny.
	Flagged int     `json:"flagged"`
	Recall  float64 `json:"recall"`
}

// PerCategoryRecall computes recall over the fixed corpus category
// vocabulary, in vocabulary order. A category with no positive-truth records
// scores recall 0 (the counts alongside make the gap explicit).
func PerCategoryRecall(scored []Scored, categories []string) []CategoryRecall {
	byCat := map[string]*CategoryRecall{}
	for _, c := range categories {
		byCat[c] = &CategoryRecall{Category: c}
	}
	for _, s := range scored {
		for _, c := range s.Record.Categories {
			cr, ok := byCat[c]
			if !ok {
				continue
			}
			cr.N++
			if isPositiveTruth(s.Record.Truth) {
				cr.TruthPositive++
				if isPositiveVerdict(s.Verdict) {
					cr.Flagged++
				}
			}
		}
	}
	out := make([]CategoryRecall, 0, len(categories))
	for _, c := range categories {
		cr := byCat[c]
		cr.Recall = ratio(cr.Flagged, cr.TruthPositive)
		out = append(out, *cr)
	}
	return out
}

// SubsetFNRFPR is the safety-view error rates broken out for one subset of
// records (architecture §7: the disguised subset scored separately).
type SubsetFNRFPR struct {
	N   int     `json:"records"`
	FNR float64 `json:"fnr"`
	FPR float64 `json:"fpr"`
}

// DisguisedView breaks out FNR and FPR over the disguised subset.
func DisguisedView(scored []Scored) SubsetFNRFPR {
	var sub []Scored
	for _, s := range scored {
		for _, c := range s.Record.Categories {
			if c == DisguisedCategory {
				sub = append(sub, s)
				break
			}
		}
	}
	c := SafetyConfusion(sub)
	return SubsetFNRFPR{N: len(sub), FNR: c.FNR(), FPR: c.FPR()}
}

// Latency summarises per-record wall-clock milliseconds.
type Latency struct {
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	Mean float64 `json:"mean"`
}

// LatencyView computes nearest-rank percentiles over the per-record latencies.
func LatencyView(ms []int64) Latency {
	n := len(ms)
	if n == 0 {
		return Latency{}
	}
	sorted := make([]int64, n)
	copy(sorted, ms)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var mean float64
	for _, v := range sorted {
		mean += float64(v)
	}
	return Latency{
		P50:  float64(sorted[nearestRank(0.50, n)-1]),
		P90:  float64(sorted[nearestRank(0.90, n)-1]),
		P95:  float64(sorted[nearestRank(0.95, n)-1]),
		P99:  float64(sorted[nearestRank(0.99, n)-1]),
		Mean: mean / float64(n),
	}
}

// nearestRank returns the 1-based nearest-rank position of percentile p in a
// sorted sample of n values: the smallest rank r with r >= p*n/100.
func nearestRank(p float64, n int) int {
	r := int(math.Ceil(p * float64(n)))
	if r < 1 {
		r = 1
	}
	if r > n {
		r = n
	}
	return r
}
