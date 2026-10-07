package systemone

// Route (architecture §5bis, "Route (thresholds tv2)"). The threshold table
// itself is a backend's operating point — each backend holds its own values,
// version string and sweep variable — while the arithmetic below is shared so
// that two backends cannot route the same measurements differently.

import (
	"fmt"
	"strconv"
	"strings"

	"tinybouncer/internal/core"
)

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

// ThresholdsConfig names one backend's threshold source: the label used in
// error messages, the sweep-override variable, and the base values the
// override applies to.
type ThresholdsConfig struct {
	// Label is the backend name the error messages name (e.g. "jev").
	Label string
	// EnvVar is the sweep override: a comma-separated list of k=v pairs.
	EnvVar string
	// Base is the backend's calibrated operating point.
	Base Thresholds
}

// LoadThresholds returns the base values with the environment override
// applied, parsed strictly: each entry must be `known_key=float`, no duplicate
// keys, no unknown keys. Any violation is a typed ConfigError so `check` exits
// 1 with a clear message.
func LoadThresholds(cfg ThresholdsConfig, lookup func(string) string) (Thresholds, error) {
	t := cfg.Base
	v := strings.TrimSpace(lookup(cfg.EnvVar))
	if v == "" {
		return t, nil
	}
	seen := make(map[string]bool)
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return Thresholds{}, &ConfigError{Message: fmt.Sprintf("%s: empty entry in %s", cfg.Label, cfg.EnvVar)}
		}
		key, val, ok := strings.Cut(part, "=")
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if !ok {
			return Thresholds{}, &ConfigError{Message: fmt.Sprintf("%s: %s entry %q must be key=value", cfg.Label, cfg.EnvVar, part)}
		}
		if seen[key] {
			return Thresholds{}, &ConfigError{Message: fmt.Sprintf("%s: duplicate key %q in %s", cfg.Label, key, cfg.EnvVar)}
		}
		seen[key] = true
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return Thresholds{}, &ConfigError{Message: fmt.Sprintf("%s: %s value of %q is not a number: %s", cfg.Label, cfg.EnvVar, key, val)}
		}
		switch key {
		case "deny_hazard":
			if err := checkHazard(cfg.Label, key, f); err != nil {
				return Thresholds{}, err
			}
			t.DenyHazard = f
		case "ask_hazard":
			if err := checkHazard(cfg.Label, key, f); err != nil {
				return Thresholds{}, err
			}
			t.AskHazard = f
		case "deny_severity":
			if err := checkSeverity(cfg.Label, key, f); err != nil {
				return Thresholds{}, err
			}
			t.DenySeverity = f
		case "ask_severity":
			if err := checkSeverity(cfg.Label, key, f); err != nil {
				return Thresholds{}, err
			}
			t.AskSeverity = f
		default:
			return Thresholds{}, &ConfigError{Message: fmt.Sprintf("%s: unknown key %q in %s (want deny_hazard, deny_severity, ask_hazard, ask_severity)", cfg.Label, key, cfg.EnvVar)}
		}
	}
	return t, nil
}

func checkHazard(label, key string, f float64) error {
	if f < 0 || f > 1 {
		return &ConfigError{Message: fmt.Sprintf("%s: %s value %v out of range (0..1)", label, key, f)}
	}
	return nil
}

func checkSeverity(label, key string, f float64) error {
	if f < 0 || f > 4 {
		return &ConfigError{Message: fmt.Sprintf("%s: %s value %v out of range (0..4)", label, key, f)}
	}
	return nil
}

// Rule identifies which gate decided the verdict (hazard gate decides first
// when both fire, matching the deny-before-ask evaluation order).
type Rule int

const (
	RuleAllow Rule = iota
	RuleHazardDeny
	RuleSeverityDeny
	RuleHazardAsk
	RuleSeverityAsk
)

// Decision is the route outcome for one command's measurements.
type Decision struct {
	// Effect is the mapped verdict effect.
	Effect core.Effect
	// Rule is the gate that fired; RuleAllow when no gate fires.
	Rule Rule
	// SeverityLevel is the inclusive minimum level of the severity gate that
	// fired (3 for deny, 2 for ask); used to derive the governing probability.
	SeverityLevel int
}

// Route applies the threshold table to the measured maxHazard and severity
// (the expected severity score), strictly deny before ask, else allow:
//
//	deny := maxHazard >= t.DenyHazard || severity >= t.DenySeverity
//	ask  := maxHazard >= t.AskHazard || severity >= t.AskSeverity
//	nothing fires ⇒ allow
func Route(t Thresholds, maxHazard, severity float64) Decision {
	if maxHazard >= t.DenyHazard || severity >= t.DenySeverity {
		if maxHazard >= t.DenyHazard {
			return Decision{Effect: core.Deny, Rule: RuleHazardDeny}
		}
		return Decision{Effect: core.Deny, Rule: RuleSeverityDeny, SeverityLevel: 3}
	}
	if maxHazard >= t.AskHazard || severity >= t.AskSeverity {
		if maxHazard >= t.AskHazard {
			return Decision{Effect: core.Ask, Rule: RuleHazardAsk}
		}
		return Decision{Effect: core.Ask, Rule: RuleSeverityAsk, SeverityLevel: 2}
	}
	return Decision{Effect: core.Allow, Rule: RuleAllow}
}
