// The doctor subcommand (architecture §3; plan T07): health reporting for
// backends, consumed by users and by the OpenCode plugin at setup. One JSON
// object per reported backend goes to stdout; human-readable notes go to
// stderr, never parsed programmatically.
//
// Doctor must never panic or block on unavailable configuration, and must
// make no network call when configuration is incomplete (a missing key is
// reported from the factory's config error before any transport is built).

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"tinybouncer/internal/backend"
)

// doctorTimeout bounds each HealthCheck so doctor cannot block indefinitely
// behind an unresponsive endpoint. Generous, but a hard stop.
const doctorTimeout = 15 * time.Second

// doctorRow is the per-backend health JSON object (plan T07). Version facts
// are empty when the backend could not even be constructed (its factory
// config error is then the informative part); error is empty when ok.
type doctorRow struct {
	Backend           string `json:"backend"`
	OK                bool   `json:"ok"`
	Model             string `json:"model"`
	PolicyVersion     string `json:"policy_version"`
	ThresholdsVersion string `json:"thresholds_version"`
	Error             string `json:"error"`
}

// runDoctor implements the doctor subcommand.
//
//	tinybouncer doctor [--backend <name>] [--json]
//
// --json is accepted for forward compatibility (architecture §8: the plugin
// runs `doctor --json`); the output is always JSON, so the flag changes
// nothing. --backend restricts the report to one backend; the default
// reports every registered backend in stable sorted order.
func runDoctor(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	backendName := fs.String("backend", "", "report only this backend (default: all registered backends)")
	jsonFlag := fs.Bool("json", false, "ignored: the output is always JSON (accepted for the plugin contract)")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	names := backend.Names()
	if *backendName != "" {
		if _, ok := backend.Lookup(*backendName); !ok {
			fmt.Fprintf(stderr, "tinybouncer doctor: unknown backend %q (available: %s)\n",
				*backendName, availableBackends())
			return 1
		}
		names = []string{*backendName}
	}
	if *jsonFlag {
		fmt.Fprintln(stderr, "tinybouncer doctor: output is always JSON; --json accepted for compatibility")
	}

	// Environment note: TINY_BOUNCER_JEV_* variables come from the process
	// environment; a stale base URL here would hit the wrong endpoint, so
	// surface what is being used when it deviates from the default.
	allOK := true
	for _, name := range names {
		row := doctorRow{Backend: name}
		factory, ok := backend.Lookup(name)
		if !ok {
			// Unreachable given the Lookup above, but fail-safe.
			row.Error = "not registered"
			allOK = false
			writeDoctorRow(stdout, stderr, row)
			fmt.Fprintf(stderr, "tinybouncer doctor: %s: not registered\n", name)
			continue
		}
		b, err := factory(backend.Config{})
		if err != nil {
			// An optional backend (api, afm) that is not configured is
			// omitted from the default report rather than failing the run;
			// an explicit --backend still surfaces its error.
			if *backendName == "" && backend.IsOptional(name) {
				continue
			}
			// Configuration unavailable (e.g. missing API key). This is
			// diagnosed before any network activity happens.
			row.Error = err.Error()
			allOK = false
			writeDoctorRow(stdout, stderr, row)
			fmt.Fprintf(stderr, "tinybouncer doctor: %s: unhealthy: %v\n", name, err)
			continue
		}
		info := b.Info()
		row.Model, row.PolicyVersion, row.ThresholdsVersion =
			info.Model, info.PolicyVersion, info.ThresholdsVersion
		ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
		err = b.HealthCheck(ctx)
		cancel()
		if err == nil {
			// A successful health check may learn facts the backend could not
			// know before it (the decider reports the checkpoint it serves from
			// GET /health), so re-read the identity before reporting it.
			info = b.Info()
			row.Model, row.PolicyVersion, row.ThresholdsVersion =
				info.Model, info.PolicyVersion, info.ThresholdsVersion
			row.OK = true
			fmt.Fprintf(stderr, "tinybouncer doctor: %s: ok (model %s, policy %s, thresholds %s)\n",
				name, row.Model, row.PolicyVersion, row.ThresholdsVersion)
		} else {
			row.Error = err.Error()
			allOK = false
			fmt.Fprintf(stderr, "tinybouncer doctor: %s: unhealthy: %v\n", name, err)
		}
		writeDoctorRow(stdout, stderr, row)
	}

	if !allOK {
		fmt.Fprintln(stderr, "tinybouncer doctor: one or more backends are unhealthy")
		return 1
	}
	return 0
}

// writeDoctorRow emits one compact JSON object per line on stdout (stable
// order: backends are reported in registry-sorted succession).
func writeDoctorRow(stdout, stderr io.Writer, row doctorRow) {
	if b, err := json.Marshal(row); err == nil {
		fmt.Fprintln(stdout, string(b))
	} else {
		// Encoding a struct of plain strings cannot fail; belt and braces.
		fmt.Fprintf(stderr, "tinybouncer doctor: internal error encoding health JSON: %v\n", err)
	}
}
