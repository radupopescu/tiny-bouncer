package jev

// Route threshold table tests (architecture §5bis "Route", thresholds tv2).
// No network: routing is a pure function of maxHazard and expected severity
// plus the thresholds.

import (
	"errors"
	"testing"
)

func TestRouteBoundaries(t *testing.T) {
	tests := []struct {
		name     string
		hazard   float64
		severity float64
		want     decision
	}{
		// Hazard deny gate at 0.85.
		{"below deny hazard gate alone", 0.84, 0, decision{effect: "ask", rule: ruleHazardAsk}},
		{"exactly at deny hazard gate", 0.85, 0, decision{effect: "deny", rule: ruleHazardDeny}},
		{"just above deny hazard gate", 0.86, 0, decision{effect: "deny", rule: ruleHazardDeny}},
		// Severity deny gate at 3.0.
		{"below deny severity gate alone", 0.1, 2.9, decision{effect: "ask", rule: ruleSeverityAsk}},
		{"exactly at deny severity gate", 0.1, 3.0, decision{effect: "deny", rule: ruleSeverityDeny}},
		{"just above deny severity gate", 0.1, 3.1, decision{effect: "deny", rule: ruleSeverityDeny}},
		// Ask gates (calibrated tv2: ask_hazard 0.80, ask_severity 1.40).
		{"just below ask hazard gate", 0.79, 0, decision{effect: "allow", rule: ruleAllow}},
		{"exactly at ask hazard gate", 0.80, 0, decision{effect: "ask", rule: ruleHazardAsk}},
		{"just above ask hazard gate", 0.81, 0, decision{effect: "ask", rule: ruleHazardAsk}},
		{"just below ask severity gate", 0.1, 1.39, decision{effect: "allow", rule: ruleAllow}},
		{"exactly at ask severity gate", 0.1, 1.40, decision{effect: "ask", rule: ruleSeverityAsk}},
		{"severity at 4 with low hazard", 0.1, 4.0, decision{effect: "deny", rule: ruleSeverityDeny}},
		// Multi-hazard: only the max carries the verdict; a low secondary does not.
		{"multi-hazard max denies", 0.86, 0, decision{effect: "deny", rule: ruleHazardDeny}},
		{"low secondary under its own gate", 0.10, 0, decision{effect: "allow", rule: ruleAllow}},
		// Allow path.
		{"fully quiet command", 0.05, 0.5, decision{effect: "allow", rule: ruleAllow}},
		{"hazards at zero severity zero", 0, 0, decision{effect: "allow", rule: ruleAllow}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := route(DefaultThresholds, tc.hazard, tc.severity)
			if got.effect != tc.want.effect || got.rule != tc.want.rule {
				t.Errorf("route(%v, %v) = %+v, want %+v", tc.hazard, tc.severity, got, tc.want)
			}
		})
	}
}

// TestRouteAskVsDeny asserts the ordering discipline of architecture §5:
// nothing ever auto-upgrades deny or ask to allow, and the deny gate is
// checked before the ask gate.
func TestRouteAskVsDeny(t *testing.T) {
	defaults := DefaultThresholds
	for _, tc := range []struct {
		name             string
		hazard, severity float64
		wantEffect       string
	}{
		{"hazard deny despite low severity", 0.99, 0.0, "deny"},
		{"severity deny despite low hazard", 0.0, 3.0, "deny"},
		{"hazard ask is not allow", 0.82, 0.0, "ask"},
		{"severity ask is not allow", 0.0, 1.45, "ask"},
	} {
		if got := route(defaults, tc.hazard, tc.severity).effect; string(got) != tc.wantEffect {
			t.Errorf("%s: effect = %s, want %s", tc.name, got, tc.wantEffect)
		}
	}
}

func lookupEnv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

// TestLoadThresholds overrides the gates: a pinned hazard probability that
// routes to ask under the defaults routes to deny with a lowered deny gate.
func TestLoadThresholdsOverride(t *testing.T) {
	t0, err := LoadThresholds(lookupEnv(nil))
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if route(t0, 0.82, 0).effect != "ask" {
		t.Fatalf("precondition: 0.82 hazard should be ask under defaults")
	}
	t1, err := LoadThresholds(lookupEnv(map[string]string{
		thresholdsEnv: "deny_hazard=0.50,deny_severity=0.0,ask_hazard=0.0,ask_severity=0.0",
	}))
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if got := route(t1, 0.60, 0).effect; got != "deny" {
		t.Errorf("effect = %s, want deny with lowered deny_hazard", got)
	}
}

func TestLoadThresholdsStrict(t *testing.T) {
	tests := []struct {
		name string
		env  string
	}{
		{"unknown key", "deny_hazrd=0.85"},
		{"malformed float", "deny_hazard=high"},
		{"missing = sign", "deny_hazard"},
		{"empty entry", "deny_hazard=0.85,,ask_hazard=0.5"},
		{"duplicate key", "ask_hazard=0.5,ask_hazard=0.4"},
		{"hazard out of range", "deny_hazard=1.5"},
		{"severity out of range", "ask_severity=9"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadThresholds(lookupEnv(map[string]string{thresholdsEnv: tc.env}))
			if err == nil {
				t.Fatalf("LoadThresholds(%q) = nil error, want ConfigError", tc.env)
			}
			var cfg *ConfigError
			if !errors.As(err, &cfg) {
				t.Fatalf("error %v is not a *ConfigError", err)
			}
		})
	}
}

// TestLoadThresholdsEmptyOverride: unset or blank override keeps the defaults.
func TestLoadThresholdsEmptyOverride(t *testing.T) {
	for _, v := range []string{"", "  "} {
		t0, err := LoadThresholds(lookupEnv(map[string]string{thresholdsEnv: v}))
		if err != nil {
			t.Fatalf("LoadThresholds(%q): %v", v, err)
		}
		if t0 != DefaultThresholds {
			t.Errorf("override %q changed the defaults: %+v", v, t0)
		}
	}
}
