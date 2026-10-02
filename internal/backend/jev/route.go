package jev

// Route thresholds (architecture §5bis, "Route (thresholds tv1)"). They live
// in code only — never in question text — so `eval --sweep` can vary them
// without changing what the model sees. An env override lets T10 calibrate
// without a rebuild.

import (
	"fmt"
	"strconv"
	"strings"

	"wiseyolo/internal/core"
)

// ThresholdsVersion is the route's thresholds version. It joins the cache key
// and meta; bump it whenever the default values change.
const ThresholdsVersion = "tv1"

// thresholdsEnv is the sweep override: a comma-separated list of k=v pairs.
const thresholdsEnv = "WISE_YOLO_JEV_THRESHOLDS"

// Thresholds holds the four route gates.
//
//	deny := maxHazard >= DenyHazard || severity >= DenySeverity
//	ask  := maxHazard >= AskHazard  || severity >= AskSeverity
//	nothing fires ⇒ allow
type Thresholds struct {
	DenyHazard   float64
	DenySeverity float64
	AskHazard    float64
	AskSeverity  float64
}

// DefaultThresholds are the initial defaults of architecture §5bis,
// to be calibrated on the live corpus (task T10).
var DefaultThresholds = Thresholds{
	DenyHazard:   0.85,
	DenySeverity: 3.0,
	AskHazard:    0.50,
	AskSeverity:  2.0,
}

// LoadThresholds reads the defaults and applies the environment override,
// parsed strictly: each entry must be `known_key=float`, no duplicate keys,
// no unknown keys. Any violation is a typed ConfigError so `check` exits 1
// with a clear message.
func LoadThresholds(lookup func(string) string) (Thresholds, error) {
	t := DefaultThresholds
	v := strings.TrimSpace(lookup(thresholdsEnv))
	if v == "" {
		return t, nil
	}
	seen := make(map[string]bool)
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return Thresholds{}, &ConfigError{Message: fmt.Sprintf("jev: empty entry in %s", thresholdsEnv)}
		}
		key, val, ok := strings.Cut(part, "=")
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if !ok {
			return Thresholds{}, &ConfigError{Message: fmt.Sprintf("jev: %s entry %q must be key=value", thresholdsEnv, part)}
		}
		if seen[key] {
			return Thresholds{}, &ConfigError{Message: fmt.Sprintf("jev: duplicate key %q in %s", key, thresholdsEnv)}
		}
		seen[key] = true
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return Thresholds{}, &ConfigError{Message: fmt.Sprintf("jev: %s value of %q is not a number: %s", thresholdsEnv, key, val)}
		}
		switch key {
		case "deny_hazard":
			if err := checkHazard(key, f); err != nil {
				return Thresholds{}, err
			}
			t.DenyHazard = f
		case "ask_hazard":
			if err := checkHazard(key, f); err != nil {
				return Thresholds{}, err
			}
			t.AskHazard = f
		case "deny_severity":
			if err := checkSeverity(key, f); err != nil {
				return Thresholds{}, err
			}
			t.DenySeverity = f
		case "ask_severity":
			if err := checkSeverity(key, f); err != nil {
				return Thresholds{}, err
			}
			t.AskSeverity = f
		default:
			return Thresholds{}, &ConfigError{Message: fmt.Sprintf("jev: unknown key %q in %s (want deny_hazard, deny_severity, ask_hazard, ask_severity)", key, thresholdsEnv)}
		}
	}
	return t, nil
}

func checkHazard(key string, f float64) error {
	if f < 0 || f > 1 {
		return &ConfigError{Message: fmt.Sprintf("jev: %s value %v out of range (0..1)", key, f)}
	}
	return nil
}

func checkSeverity(key string, f float64) error {
	if f < 0 || f > 4 {
		return &ConfigError{Message: fmt.Sprintf("jev: %s value %v out of range (0..4)", key, f)}
	}
	return nil
}

// routeRule identifies which gate decided the verdict (hazard gate decides
// first when both fire, matching the deny-before-ask evaluation order).
type routeRule int

const (
	ruleAllow routeRule = iota
	ruleHazardDeny
	ruleSeverityDeny
	ruleHazardAsk
	ruleSeverityAsk
)

// decision is the route outcome for one command's measurements.
type decision struct {
	// effect is the mapped verdict effect.
	effect core.Effect
	// rule is the gate that fired; ruleAllow when no gate fires.
	rule routeRule
	// severityLevel is the inclusive minimum level of the severity gate that
	// fired (3 for deny, 2 for ask); used to derive the governing probability.
	severityLevel int
}

// route applies the threshold table to the measured maxHazard and severity
// (the expected severity score), strictly deny before ask, else allow:
//
//	deny := maxHazard >= t.DenyHazard || severity >= t.DenySeverity
//	ask  := maxHazard >= t.AskHazard || severity >= t.AskSeverity
//	nothing fires ⇒ allow
func route(t Thresholds, maxHazard, severity float64) decision {
	if maxHazard >= t.DenyHazard || severity >= t.DenySeverity {
		if maxHazard >= t.DenyHazard {
			return decision{effect: core.Deny, rule: ruleHazardDeny}
		}
		return decision{effect: core.Deny, rule: ruleSeverityDeny, severityLevel: 3}
	}
	if maxHazard >= t.AskHazard || severity >= t.AskSeverity {
		if maxHazard >= t.AskHazard {
			return decision{effect: core.Ask, rule: ruleHazardAsk}
		}
		return decision{effect: core.Ask, rule: ruleSeverityAsk, severityLevel: 2}
	}
	return decision{effect: core.Allow, rule: ruleAllow}
}
