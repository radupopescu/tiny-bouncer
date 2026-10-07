package main_test

// Subprocess contract tests for the doctor subcommand (architecture §3;
// plan T07 criteria). The binary is built once by TestMain in main_test.go.
// All network coverage is via httptest servers only (plan §2: tests never
// touch the network).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doctorHealth is the per-backend health JSON object on stdout (T07).
type doctorHealth struct {
	Backend           string `json:"backend"`
	OK                bool   `json:"ok"`
	Model             string `json:"model"`
	PolicyVersion     string `json:"policy_version"`
	ThresholdsVersion string `json:"thresholds_version"`
	Error             string `json:"error"`
}

// parseDoctorRows splits doctor's stdout (one JSON object per backend) and
// decodes each line, failing on anything malformed.
func parseDoctorRows(t *testing.T, stdout string) []doctorHealth {
	t.Helper()
	var rows []doctorHealth
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line == "" {
			continue
		}
		var h doctorHealth
		if err := json.Unmarshal([]byte(line), &h); err != nil {
			t.Fatalf("stdout line is not health JSON: %v\nline: %s", err, line)
		}
		rows = append(rows, h)
	}
	return rows
}

// jevEnv builds the environment for a doctor run against a test endpoint.
func jevEnv(baseURL string) []string {
	return []string{"TINY_BOUNCER_JEV_BASE_URL=" + baseURL, "TINY_BOUNCER_JEV_API_KEY=test-key"}
}

func TestDoctorHealthyJev(t *testing.T) {
	// /v1/models answers 200; no classify traffic is expected.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	code, stdout, _ := runBinary(t, checkBin, "", jevEnv(srv.URL), "doctor")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stdout=%q", code, stdout)
	}
	rows := parseDoctorRows(t, stdout)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (jev, mock)", len(rows))
	}
	// Stable sorted order: jev first, mock second.
	if rows[0].Backend != "jev" || rows[1].Backend != "mock" {
		t.Fatalf("order = %v, %v; want jev then mock", rows[0].Backend, rows[1].Backend)
	}
	j := rows[0]
	if !j.OK || j.Error != "" {
		t.Errorf("jev row = %+v; want ok with empty error", j)
	}
	if j.Model == "" || j.PolicyVersion != "jev-policy-1.0" || j.ThresholdsVersion != "tv2" {
		t.Errorf("jev version facts wrong: %+v", j)
	}
	m := rows[1]
	if !m.OK || m.Model != "mock-rules" || m.PolicyVersion != "mock-0" || m.ThresholdsVersion != "mock-0" {
		t.Errorf("mock row = %+v; want always-ok mock facts", m)
	}
}

func TestDoctor401ExitsOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	code, stdout, _ := runBinary(t, checkBin, "", jevEnv(srv.URL), "doctor")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 on a 401 health check", code)
	}
	rows := parseDoctorRows(t, stdout)
	var j *doctorHealth
	for i := range rows {
		if rows[i].Backend == "jev" {
			j = &rows[i]
		}
	}
	if j == nil {
		t.Fatalf("no jev row in stdout: %s", stdout)
	}
	if j.OK || j.Error == "" {
		t.Errorf("jev row = %+v; want ok:false with a populated error", j)
	}
	// The mock backend is unaffected by jev's credentials.
	if rows[len(rows)-1].Backend != "mock" || !rows[len(rows)-1].OK {
		t.Errorf("mock row must remain ok: %+v", rows[len(rows)-1])
	}
}

func TestDoctorMockAlwaysOK(t *testing.T) {
	code, stdout, _ := runBinary(t, checkBin, "", nil, "doctor", "--backend", "mock")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stdout=%q", code, stdout)
	}
	rows := parseDoctorRows(t, stdout)
	if len(rows) != 1 || rows[0].Backend != "mock" || !rows[0].OK {
		t.Fatalf("rows = %+v; want exactly one ok mock row", rows)
	}
}

func TestDoctorUnknownBackendExitsOne(t *testing.T) {
	code, stdout, stderr := runBinary(t, checkBin, "", nil, "doctor", "--backend", "nope")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for an unknown backend", code)
	}
	if strings.Contains(stdout, `"backend"`) {
		t.Errorf("no health JSON may be emitted for an unknown backend; stdout=%q", stdout)
	}
	if !strings.Contains(stderr, "unknown backend") || !strings.Contains(stderr, "nope") {
		t.Errorf("stderr must name the unknown backend; stderr=%q", stderr)
	}
}

func TestDoctorMissingKeyExitsOneWithoutNetwork(t *testing.T) {
	// Start a server, then close it: any HTTP attempt against the closed
	// endpoint would exercise the network path and fail with a connection
	// error. The missing-key path must fail before any such attempt.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	url := srv.URL
	srv.Close()

	env := []string{"TINY_BOUNCER_JEV_BASE_URL=" + url}
	code, stdout, _ := runBinary(t, checkBin, "", env, "doctor")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 with no key configured", code)
	}
	rows := parseDoctorRows(t, stdout)
	var j *doctorHealth
	for i := range rows {
		if rows[i].Backend == "jev" {
			j = &rows[i]
		}
	}
	if j == nil {
		t.Fatalf("no jev row in stdout: %s", stdout)
	}
	if j.OK || !strings.Contains(j.Error, "no API key") {
		t.Errorf("jev row = %+v; want ok:false with an informative missing-key error", j)
	}
	if strings.Contains(strings.ToLower(j.Error), "unreachable") ||
		strings.Contains(strings.ToLower(j.Error), "connect") {
		t.Errorf("error %q suggests a network attempt was made against a closed endpoint", j.Error)
	}
}

func TestDoctorUnreachableEndpointExitsOne(t *testing.T) {
	// With a key present but a dead endpoint, doctor degrades to exit 1 with
	// an informative reachability error, never a panic.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()

	code, stdout, _ := runBinary(t, checkBin, "", jevEnv(url), "doctor")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for an unreachable endpoint", code)
	}
	rows := parseDoctorRows(t, stdout)
	var j *doctorHealth
	for i := range rows {
		if rows[i].Backend == "jev" {
			j = &rows[i]
		}
	}
	if j == nil {
		t.Fatalf("no jev row in stdout: %s", stdout)
	}
	if j.OK {
		t.Errorf("jev must not report ok against a closed endpoint")
	}
	if j.Error == "" {
		t.Errorf("reachability error must be populated; row = %+v", j)
	}
}

// TestDoctorAPI exercises the api backend's doctor path against httptest: a
// /models 200 is healthy; a 401 is unhealthy. No network beyond httptest.
func TestDoctorAPI(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer healthy.Close()

	env := []string{"TINY_BOUNCER_API_BASE_URL=" + healthy.URL, "TINY_BOUNCER_API_MODEL=test-model"}
	code, stdout, stderr := runBinary(t, checkBin, "", env, "doctor", "--backend", "api")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	rows := parseDoctorRows(t, stdout)
	if len(rows) != 1 || rows[0].Backend != "api" || !rows[0].OK {
		t.Fatalf("rows = %+v; want exactly one ok api row", rows)
	}

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer broken.Close()
	code, stdout, _ = runBinary(t, checkBin, "",
		[]string{"TINY_BOUNCER_API_BASE_URL=" + broken.URL, "TINY_BOUNCER_API_MODEL=test-model"},
		"doctor", "--backend", "api")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a 401 health check", code)
	}
	rows = parseDoctorRows(t, stdout)
	if len(rows) != 1 || rows[0].OK || rows[0].Error == "" {
		t.Fatalf("rows = %+v; want one unhealthy api row with an error", rows)
	}
}

// TestDoctorAFM exercises the afm backend's doctor path with a fake `fm`:
// available → exit 0; modelNotReady → exit 1. No Apple Intelligence needed.
func TestDoctorAFM(t *testing.T) {
	fakeFM := filepath.Join("..", "..", "internal", "backend", "chat", "testdata", "fakefm.sh")
	code, stdout, stderr := runBinary(t, checkBin, "",
		[]string{"TINY_BOUNCER_AFM_EXECUTABLE=" + fakeFM}, "doctor", "--backend", "afm")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	rows := parseDoctorRows(t, stdout)
	if len(rows) != 1 || rows[0].Backend != "afm" || !rows[0].OK {
		t.Fatalf("rows = %+v; want exactly one ok afm row", rows)
	}

	unready := filepath.Join(t.TempDir(), "fm.sh")
	if err := os.WriteFile(unready,
		[]byte("#!/bin/sh\necho 'System model unavailable: modelNotReady'\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	code, stdout, _ = runBinary(t, checkBin, "",
		[]string{"TINY_BOUNCER_AFM_EXECUTABLE=" + unready}, "doctor", "--backend", "afm")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 when the model is not ready", code)
	}
	rows = parseDoctorRows(t, stdout)
	if len(rows) != 1 || rows[0].OK || !strings.Contains(rows[0].Error, "modelNotReady") {
		t.Fatalf("rows = %+v; want one unhealthy afm row naming modelNotReady", rows)
	}
}

// TestDoctorDecider exercises the decider backend's doctor path against
// httptest: a /health 200 is healthy and names the served checkpoint; a 500 is
// unhealthy; unconfigured, the optional backend is omitted from the default
// report but an explicit --backend still surfaces it. No network beyond
// httptest.
func TestDoctorDecider(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"status":"ok","model":"strands-decider-2B-hobson-v21","device":"mps"}`)
	}))
	defer healthy.Close()

	env := []string{"TINY_BOUNCER_DECIDER_BASE_URL=" + healthy.URL}
	code, stdout, stderr := runBinary(t, checkBin, "", env, "doctor", "--backend", "decider")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	rows := parseDoctorRows(t, stdout)
	if len(rows) != 1 || rows[0].Backend != "decider" || !rows[0].OK {
		t.Fatalf("rows = %+v; want exactly one ok decider row", rows)
	}
	if rows[0].Model != "strands-decider-2B-hobson-v21" || rows[0].PolicyVersion != "jev-policy-1.0" ||
		rows[0].ThresholdsVersion != "dtv1" {
		t.Errorf("decider facts = %+v; want the health model, the shared policy and dtv1", rows[0])
	}

	// Unconfigured: omitted from the default report, reported explicitly.
	code, stdout, _ = runBinary(t, checkBin, "", []string{}, "doctor")
	rows = parseDoctorRows(t, stdout)
	for _, r := range rows {
		if r.Backend == "decider" {
			t.Errorf("an unconfigured decider must be omitted from the default report: %+v", r)
		}
	}
	code, stdout, _ = runBinary(t, checkBin, "", []string{}, "doctor", "--backend", "decider")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for an unconfigured backend", code)
	}
	rows = parseDoctorRows(t, stdout)
	if len(rows) != 1 || rows[0].OK || !strings.Contains(rows[0].Error, "TINY_BOUNCER_DECIDER_BASE_URL") {
		t.Fatalf("rows = %+v; want one unhealthy row naming the variable", rows)
	}

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()
	code, stdout, _ = runBinary(t, checkBin, "",
		[]string{"TINY_BOUNCER_DECIDER_BASE_URL=" + broken.URL}, "doctor", "--backend", "decider")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a 500 health check", code)
	}
	rows = parseDoctorRows(t, stdout)
	if len(rows) != 1 || rows[0].OK || !strings.Contains(rows[0].Error, "500") {
		t.Fatalf("rows = %+v; want one unhealthy row naming the status", rows)
	}
}
