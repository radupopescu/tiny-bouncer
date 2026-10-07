package systemone

// The shared threshold-parse path is exercised through each backend's own
// LoadThresholds (see internal/backend/jev/route.go); this test pins the
// property those backends depend on: the variable name and the base values
// both come from the caller, so a second backend cannot inherit Jev's
// override variable or Jev's operating point by accident.

import (
	"errors"
	"strings"
	"testing"
)

func lookupEnv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func TestLoadThresholdsUsesCallerVariableAndBase(t *testing.T) {
	base := Thresholds{DenyHazard: 0.9, DenySeverity: 3.5, AskHazard: 0.7, AskSeverity: 1.1}
	cfg := ThresholdsConfig{Label: "other", EnvVar: "TINY_BOUNCER_OTHER_THRESHOLDS", Base: base}

	// No override for this backend's variable: the caller's base is returned
	// unchanged, even when another backend's variable is set.
	got, err := LoadThresholds(cfg, lookupEnv(map[string]string{"TINY_BOUNCER_JEV_THRESHOLDS": "deny_hazard=0.10"}))
	if err != nil {
		t.Fatalf("LoadThresholds: %v", err)
	}
	if got != base {
		t.Errorf("base = %+v, want the caller's %+v (another backend's variable must not apply)", got, base)
	}

	// The caller's variable applies, on top of the caller's base.
	got, err = LoadThresholds(cfg, lookupEnv(map[string]string{
		"TINY_BOUNCER_OTHER_THRESHOLDS": "deny_hazard=0.10",
	}))
	if err != nil {
		t.Fatalf("LoadThresholds override: %v", err)
	}
	want := base
	want.DenyHazard = 0.10
	if got != want {
		t.Errorf("override = %+v, want %+v", got, want)
	}

	// A violation names the caller's label and variable.
	_, err = LoadThresholds(cfg, lookupEnv(map[string]string{
		"TINY_BOUNCER_OTHER_THRESHOLDS": "nope=1",
	}))
	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("error %v is not a *ConfigError", err)
	}
	for _, want := range []string{"other", "TINY_BOUNCER_OTHER_THRESHOLDS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}
