package decider

// The decider's operating point. The threshold arithmetic and the strict
// override parser are shared (internal/backend/systemone); what lives here is
// this backend's own business: its values, its version and its sweep variable.

import "tinybouncer/internal/backend/systemone"

// ThresholdsVersion is the route's thresholds version. It joins the cache key
// and meta; bump it whenever the default values change.
const ThresholdsVersion = "dtv1"

// thresholdsEnv is the sweep override: a comma-separated list of k=v pairs.
const thresholdsEnv = "TINY_BOUNCER_DECIDER_THRESHOLDS"

// DefaultThresholds start from Jev's calibrated tv2 point (task T10): the
// battery and the route arithmetic are the same, so the same gates are the
// honest starting point. The decider's probabilities are not Jev's, though, so
// these values are a placeholder until task T20 sweeps them; T20 commits the
// calibrated values and bumps the version if they move.
var DefaultThresholds = systemone.Thresholds{
	DenyHazard:   0.85,
	DenySeverity: 3.0,
	AskHazard:    0.80,
	AskSeverity:  1.40,
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
