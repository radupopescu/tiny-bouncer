package jev

// Jev's operating point (architecture §5bis, "Route (thresholds tv2)"). The
// threshold arithmetic and the strict override parser are shared
// (internal/backend/systemone); what stays here is Jev's own business: the
// calibrated values, the thresholds version, and the sweep variable.
//
// Thresholds live in code only — never in question text — so `eval --sweep`
// can vary them without changing what the model sees.

import "tinybouncer/internal/backend/systemone"

// ThresholdsVersion is the route's thresholds version. It joins the cache key
// and meta; bump it whenever the default values change.
const ThresholdsVersion = "tv2"

// thresholdsEnv is the sweep override: a comma-separated list of k=v pairs.
const thresholdsEnv = "TINY_BOUNCER_JEV_THRESHOLDS"

// DefaultThresholds were calibrated on the live corpus in task T10
// (eval --sweep over the 258-record synthetic battery, multiple live runs).
// The ask gates moved from the tv1 starting point: ask_hazard 0.50 → 0.80
// (the 0.50–0.80 hazard band triggered on routine safe build/test commands
// such as `npm test` and `cargo build`), and ask_severity 2.0 → 1.40 (a
// severity expectation ≥ 1.4 is where borderline work — `git revert HEAD`,
// `chmod -R 750 ./internal` — and quiet history-rewriting like
// `git lfs migrate export --everything` sits).
var DefaultThresholds = systemone.Thresholds{
	DenyHazard:   0.85,
	DenySeverity: 3.0,
	AskHazard:    0.80,
	AskSeverity:  1.40,
}

// LoadThresholds reads the defaults and applies the environment override. The
// parser is shared; Jev supplies its own label, variable and base values.
func LoadThresholds(lookup func(string) string) (systemone.Thresholds, error) {
	return systemone.LoadThresholds(systemone.ThresholdsConfig{
		Label:  "jev",
		EnvVar: thresholdsEnv,
		Base:   DefaultThresholds,
	}, lookup)
}
