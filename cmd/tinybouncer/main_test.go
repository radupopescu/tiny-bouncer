package main_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"tinybouncer/internal/dispatch"
)

// This file holds the subprocess contract tests for the check output
// contract (architecture §3 internal/backend mock-backed, plan T03 criteria).
// The binaries are built once by TestMain into a test-only scratch dir; the
// failcheck harness (internal/dispatch/testdata/failcheck) provides a backend
// whose Classify fails so the unjudged→ask filling is exercised as a
// subprocess too.

var checkBin, failBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tinybouncer-contract")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	build := func(out, target string) {
		cmd := exec.Command("go", "build", "-o", out, target)
		if b, err2 := cmd.CombinedOutput(); err2 != nil {
			panic("go build " + target + ": " + string(b))
		}
	}
	checkBin = filepath.Join(dir, "tinybouncer")
	failBin = filepath.Join(dir, "failcheck")
	build(checkBin, ".")
	build(failBin, filepath.Join("..", "..", "internal", "dispatch", "testdata", "failcheck"))
	os.Exit(m.Run())
}

// scrubKeys removes every Jev credential, backend-selection and cache variable
// from an environment copy, so contract tests do not depend on the
// developer's key configuration (and never touch the developer's real cache).
func scrubKeys(env []string) []string {
	var out []string
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "TYPESAFE_API_KEY="),
			strings.HasPrefix(kv, "TINY_BOUNCER_JEV_API_KEY="),
			strings.HasPrefix(kv, "TINY_BOUNCER_BACKEND="),
			strings.HasPrefix(kv, "TINY_BOUNCER_CACHE="),
			strings.HasPrefix(kv, "TINY_BOUNCER_CACHE_DIR="),
			strings.HasPrefix(kv, "TINY_BOUNCER_JEV_THRESHOLDS="):
		default:
			out = append(out, kv)
		}
	}
	return out
}

// contract is the parsed output contract of check (architecture §3).
type contract struct {
	Results []struct {
		Command    string   `json:"command"`
		Verdict    string   `json:"verdict"`
		Confidence float64  `json:"confidence"`
		Categories []string `json:"categories"`
		Reason     string   `json:"reason"`
	} `json:"results"`
	Aggregate struct {
		Effect string `json:"effect"`
		Reason string `json:"reason"`
	} `json:"aggregate"`
	Meta struct {
		Backend           string `json:"backend"`
		BackendModel      string `json:"backend_model"`
		PolicyVersion     string `json:"policy_version"`
		ThresholdsVersion string `json:"thresholds_version"`
		WallMS            int64  `json:"wall_ms"`
		Cached            bool   `json:"cached"`
		Attempts          string `json:"attempts"`
	} `json:"meta"`
}

// fixture reads a JSON fixture from the shared internal/dispatch testdata.
func fixture(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join("..", "..", "internal", "dispatch", "testdata", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(b)
}

// runBinary runs a built binary with stdin, returning exit code and streams.
func runBinary(t *testing.T, bin, stdin string, env []string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running %s: %v", bin, err)
	}
	return code, stdout.String(), stderr.String()
}

func parse(t *testing.T, stdout string) contract {
	t.Helper()
	var c contract
	if err := json.Unmarshal([]byte(stdout), &c); err != nil {
		t.Fatalf("stdout is not contract JSON: %v\n%s", err, stdout)
	}
	return c
}

func TestCheckBinaryHappyPath(t *testing.T) {
	code, stdout, stderr := runBinary(t, checkBin, fixture(t, "happy.json"), nil, "check", "--backend", "mock")
	if code != 0 {
		t.Fatalf("exit = %d, stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stderr != "" {
		t.Fatalf("stdout-only contract violated; stderr = %q", stderr)
	}
	c := parse(t, stdout)
	if len(c.Results) != 2 {
		t.Fatalf("results = %d, want 2 index-aligned rows", len(c.Results))
	}
	if c.Results[0].Command != "git status" || c.Results[1].Command != "rm -rf /" {
		t.Fatalf("index alignment broken: %+v", c.Results)
	}
	if c.Results[0].Verdict != "allow" {
		t.Errorf("git status → %q, want allow (mock safe prefix)", c.Results[0].Verdict)
	}
	if c.Results[1].Verdict != "deny" {
		t.Errorf("rm -rf / → %q, want deny (mock dangerous signature)", c.Results[1].Verdict)
	}
	if c.Aggregate.Effect != "deny" {
		t.Errorf("aggregate = %q, want deny", c.Aggregate.Effect)
	}
	for i, r := range c.Results {
		if r.Categories == nil {
			t.Errorf("result %d categories null; must marshal as []", i)
		}
		if r.Reason == "" {
			t.Errorf("result %d reason empty", i)
		}
	}
}

func TestCheckBinaryEmptyBatch(t *testing.T) {
	code, stdout, stderr := runBinary(t, checkBin, fixture(t, "empty.json"), nil, "check", "--backend", "mock")
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "\"results\": []") {
		t.Errorf("empty results must marshal as [], got: %s", stdout)
	}
	c := parse(t, stdout)
	if len(c.Results) != 0 {
		t.Fatalf("results = %d, want 0", len(c.Results))
	}
	if c.Aggregate.Effect != "allow" {
		t.Errorf("empty-batch aggregate = %q, want allow", c.Aggregate.Effect)
	}
}

func TestCheckBinaryMetaFactsFromBackendInfo(t *testing.T) {
	_, stdout, _ := runBinary(t, checkBin, fixture(t, "empty.json"), scrubKeys(os.Environ()), "check", "--backend", "mock", "--no-cache")
	m := parse(t, stdout).Meta
	if m.Backend != "mock" || m.BackendModel != "mock-rules" ||
		m.PolicyVersion != "mock-0" || m.ThresholdsVersion != "mock-0" {
		t.Fatalf("meta backend facts = %+v; must equal the mock backend's Info", m)
	}
	if m.WallMS < 0 {
		t.Fatalf("wall_ms = %d, want >= 0", m.WallMS)
	}
	if m.Cached {
		t.Error("empty batch cannot report cached=true: there is nothing to serve")
	}
	if m.Attempts != "1" {
		t.Fatalf("attempts = %q, want 1", m.Attempts)
	}
}

func TestCheckBinaryOversizedInput(t *testing.T) {
	commands := make([]string, dispatch.MaxCommands+1)
	for i := range commands {
		commands[i] = "echo hi"
	}
	raw, err := json.Marshal(map[string][]string{"commands": commands})
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runBinary(t, checkBin, string(raw), nil, "check", "--backend", "mock")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (usage/config)", code)
	}
	if !strings.Contains(stderr, "exceeds the limit") {
		t.Fatalf("stderr = %q; want a size diagnostic", stderr)
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(stdout), &e); err != nil || !strings.Contains(e.Error, "exceeds") {
		t.Fatalf("stdout = %q; want a valid JSON error object diagnosing the size", stdout)
	}
}

func TestCheckBinaryUnknownBackend(t *testing.T) {
	code, stdout, stderr := runBinary(t, checkBin, fixture(t, "empty.json"), nil, "check", "--backend", "nope")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if strings.Contains(stdout, "\"meta\"") {
		t.Errorf("unknown backend must not emit a contract on stdout")
	}
	if !strings.Contains(stderr, "unknown backend") || !strings.Contains(stderr, "mock") {
		t.Fatalf("stderr = %q; want unknown-backend diagnostic listing mock", stderr)
	}
}

func TestCheckBinaryMalformedStdin(t *testing.T) {
	for _, bad := range []string{"", "{", "{\"commands\": \"not-an-array\"}"} {
		code, stdout, stderr := runBinary(t, checkBin, bad, nil, "check", "--backend", "mock")
		if code != 1 {
			t.Errorf("malformed %q: exit = %d, want 1", bad, code)
		}
		if stdout != "" {
			var e struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal([]byte(stdout), &e); err != nil || e.Error == "" {
				t.Errorf("malformed %q: stdout %q is neither empty nor a JSON error object", bad, stdout)
			}
		}
		if strings.Contains(stdout, "\"meta\"") {
			t.Errorf("malformed %q produced a contract on stdout", bad)
		}
		if stderr == "" {
			t.Errorf("malformed %q: no stderr diagnostic", bad)
		}
	}
}

func TestCheckBinaryRouting(t *testing.T) {
	code, _, stderr := runBinary(t, checkBin, "", nil, "bogus")
	if code != 1 || !strings.Contains(stderr, "usage") {
		t.Fatalf("bogus subcommand: exit=%d stderr=%q, want exit 1 with usage on stderr", code, stderr)
	}
	code, _, stderr = runBinary(t, checkBin, "", nil)
	if code != 1 || !strings.Contains(stderr, "usage") {
		t.Fatalf("missing subcommand: exit=%d stderr=%q, want exit 1 with usage on stderr", code, stderr)
	}
	// Amendment (T07): doctor is implemented now; with no Jev key visible it
	// exits 1 across the default backend, and it must not emit a check
	// contract on stdout. The key variables are scrubbed from the
	// environment so the assertion does not depend on the developer's setup.
	code, stdout, stderr := runBinary(t, checkBin, "", scrubKeys(os.Environ()), "doctor")
	if code != 1 || !strings.Contains(stderr, "doctor") {
		t.Fatalf("doctor: exit=%d stderr=%q, want exit 1 with a doctor diagnostic", code, stderr)
	}
	if strings.Contains(stdout, "\"meta\"") {
		t.Errorf("doctor must not emit a check contract on stdout")
	}
	for _, sc := range []string{"bogus-subcommand"} {
		code, stdout, stderr := runBinary(t, checkBin, "", nil, sc)
		if code != 1 || !strings.Contains(stderr, "usage") {
			t.Fatalf("%s: exit=%d stderr=%q, want exit 1 with a usage message", sc, code, stderr)
		}
		if strings.Contains(stdout, "\"meta\"") {
			t.Errorf("%s must not emit a contract on stdout", sc)
		}
	}
}

func TestCheckBinaryBackendEnvDefaultHonoured(t *testing.T) {
	env := append(os.Environ(), "TINY_BOUNCER_BACKEND=nope")
	code, _, stderr := runBinary(t, checkBin, fixture(t, "empty.json"), env, "check")
	if code != 1 || !strings.Contains(stderr, "unknown backend") {
		t.Fatalf("exit=%d stderr=%q; TINY_BOUNCER_BACKEND must set the flag default", code, stderr)
	}
}

func TestFailcheckUnjudgedAsk(t *testing.T) {
	code, stdout, stderr := runBinary(t, failBin, "{\"commands\":[\"echo hi\",\"rm -rf /\"]}", nil)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%q; degraded verdicts stay exit 0 (the JSON is the contract)", code, stderr)
	}
	c := parse(t, stdout)
	if len(c.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(c.Results))
	}
	for i, r := range c.Results {
		if r.Verdict != "ask" {
			t.Errorf("result %d verdict = %q, want ask (failure effect)", i, r.Verdict)
		}
		if !strings.Contains(r.Reason, "unjudged") || !strings.Contains(r.Reason, "connection refused") {
			t.Errorf("result %d reason %q must name the failure mode", i, r.Reason)
		}
	}
	if c.Aggregate.Effect != "ask" || c.Aggregate.Reason == "" {
		t.Fatalf("aggregate = %+v, want ask with the failure reason", c.Aggregate)
	}
	if c.Meta.Backend != "failing" || c.Meta.BackendModel != "fail-model" ||
		c.Meta.PolicyVersion != "fail-0" || c.Meta.ThresholdsVersion != "fail-0" {
		t.Fatalf("meta = %+v", c.Meta)
	}
}

// The flags are effective since task T04: --cache/--no-cache override the
// TINY_BOUNCER_CACHE default. This test pins the override semantics with a
// test-local cache directory.
func TestCheckBinaryCacheFlagsOverride(t *testing.T) {
	root := t.TempDir()
	env := append(scrubKeys(os.Environ()),
		"TINY_BOUNCER_CACHE_DIR="+root,
		"TINY_BOUNCER_CACHE=false")
	stdin := fixture(t, "happy.json")
	// Env says off; --cache forces it on. First run is cold, second cached.
	code, stdout, stderr := runBinary(t, checkBin, stdin, env, "check", "--backend", "mock", "--cache")
	if code != 0 {
		t.Fatalf("--cache: exit=%d stderr=%q", code, stderr)
	}
	if parse(t, stdout).Meta.Cached {
		t.Error("--cache first run must still be cold: cached=true")
	}
	code, stdout, _ = runBinary(t, checkBin, stdin, env, "check", "--backend", "mock", "--cache")
	if code != 0 || !parse(t, stdout).Meta.Cached {
		t.Fatalf("--cache second run: exit=%d, want cached=true", code)
	}
	// --no-cache overrides a populated cache.
	envOn := append(env, "TINY_BOUNCER_CACHE=true")
	code, stdout, _ = runBinary(t, checkBin, stdin, envOn, "check", "--backend", "mock", "--no-cache")
	if code != 0 {
		t.Fatalf("--no-cache: exit=%d", code)
	}
	if parse(t, stdout).Meta.Cached {
		t.Error("--no-cache must report cached=false even with entries present")
	}
	code, _, stderr = runBinary(t, checkBin, stdin, env, "check", "--backend", "mock", "--cache", "--no-cache")
	if code != 1 || stderr == "" {
		t.Fatalf("conflicting cache flags: exit=%d stderr=%q, want exit 1", code, stderr)
	}
}
