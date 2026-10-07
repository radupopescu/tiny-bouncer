package decider

// The decider's operating point. The threshold arithmetic and the strict
// override parser are shared (internal/backend/systemone); what lives here is
// this backend's own business: its values, its version and its sweep variable.

import "tinybouncer/internal/backend/systemone"

// ThresholdsVersion is the route's thresholds version. It joins the cache key
// and meta; bump it whenever the default values change.
const ThresholdsVersion = "dtv2"

// thresholdsEnv is the sweep override: a comma-separated list of k=v pairs.
const thresholdsEnv = "TINY_BOUNCER_DECIDER_THRESHOLDS"

// DefaultThresholds were calibrated in task T20 (2026-10-07) on the 265-record
// synthetic corpus, cache off, one request per command, concurrency 1, against
// the v21 checkpoint served locally (strands-decider 0.1.0, torch 2.14.1,
// transformers 5.19.0, MPS).
//
// Method: a four-variant live sweep showed that Jev's tv2 values are not
// transferable — the decider's severity distribution is compressed (safe
// commands score a median expected severity of 1.51, so tv2's ask_severity
// 1.40 flags every safe command), and carrying tv2 over leaves FNR 0.0175
// (3 of 171 dangerous commands auto-allowed). A grid search over the raw
// battery answers recorded for all 265 commands (the routing is a pure function
// of max hazard and expected severity) then mapped the full frontier. Under the
// preregistered rule — hard FNR = 0 first, then minimum FPR, then maximum
// three-way accuracy — the best point is:
//
//	TP 171 · FN 0 · FP 39 · TN 55 · sensitivity 1.000 · specificity 0.585
//	F1 0.898 · FNR 0.000 · FPR 0.415 · accuracy3 0.766 · p50 2.7 s · p95 2.9 s
//
// The tie set at those metrics contains configurations without a hazard ask
// band (ask_hazard ≥ deny_hazard); this one keeps a real ask band and the
// smaller deny gate, which is the more conservative choice. The decider is
// still **comparison-only**: it interrupts 41.5 % of safe commands against
// Jev's 13.8 %, and its `ask` class is nearly unused (6 of 22 ask truths).
var DefaultThresholds = systemone.Thresholds{
	DenyHazard:   0.65,
	DenySeverity: 1.80,
	AskHazard:    0.45,
	AskSeverity:  1.60,
}

// LoadThresholds reads the defaults and applies the environment override. The
// parser is shared; the decider supplies its own label, variable and base
// values.
func LoadThresholds(lookup func(string) string) (systemone.Thresholds, error) {
	return systemone.LoadThresholds(systemone.ThresholdsConfig{
		Label:  "decider",
		EnvVar: thresholdsEnv,
		Base:   DefaultThresholds,
	}, lookup)
}
