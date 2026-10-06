package chat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wiseyolo/internal/backend"
	"wiseyolo/internal/core"
)

// afmBackend builds the afm backend against a given executable via the
// registered factory, without touching the process environment.
func afmBackend(t *testing.T, executable string) (backend.Backend, error) {
	t.Helper()
	factory, ok := backend.Lookup("afm")
	if !ok {
		t.Fatal("afm backend is not registered")
	}
	return factory(backend.Config{Values: map[string]string{afmExecEnv: executable}})
}

// script writes an executable shim and returns its path.
func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fm.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	return path
}

func TestAFMClassifyBatch(t *testing.T) {
	b, err := afmBackend(t, filepath.Join("testdata", "fakefm.sh"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if b.Name() != "afm" {
		t.Fatalf("name = %q, want afm", b.Name())
	}
	cmds := []core.Command{{Raw: "git status"}, {Raw: "rm -rf /"}, {Raw: "ls"}}
	got, err := b.Classify(context.Background(), cmds)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if len(got) != len(cmds) {
		t.Fatalf("verdicts = %d, want %d index-aligned", len(got), len(cmds))
	}
	for i, v := range got {
		if v.Effect != core.Deny {
			t.Errorf("verdict[%d].effect = %q, want deny", i, v.Effect)
		}
		if len(v.Categories) != 1 || v.Categories[0] != "fs_destructive" {
			t.Errorf("verdict[%d].categories = %v, want [fs_destructive] (bogus dropped)", i, v.Categories)
		}
		if !strings.Contains(v.Reason, "fake fm deny") {
			t.Errorf("verdict[%d].reason = %q, want the model reason", i, v.Reason)
		}
	}
	info := b.Info()
	if info.PolicyVersion != PolicyVersion || info.ThresholdsVersion != ThresholdsVersion || info.Model != "system" {
		t.Errorf("Info = %+v, want policy %s thresholds %s model system", info, PolicyVersion, ThresholdsVersion)
	}
}

func TestAFMOneProcessPerCommand(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "count")
	shim := script(t, fmt.Sprintf("echo x >> %q\ncat >/dev/null\nprintf '%%s\\n' '{\"effect\":\"ask\",\"confidence\":0.5,\"categories\":[],\"reason\":\"r\"}'\n", counter))
	b, err := afmBackend(t, shim)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if _, err := b.Classify(context.Background(), []core.Command{{Raw: "a"}, {Raw: "b"}, {Raw: "c"}}); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if n := strings.Count(string(data), "x"); n != 3 {
		t.Errorf("invocations = %d, want 3 (one process per command)", n)
	}
}

func TestAFMFailureModesBecomeAsk(t *testing.T) {
	shim := script(t, "cat >/dev/null\necho boom >&2\nexit 3\n")
	b, err := afmBackend(t, shim)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	got, err := b.Classify(context.Background(), []core.Command{{Raw: "anything"}})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got[0].Effect != core.Ask || !strings.Contains(got[0].Reason, "afm: command unjudged") {
		t.Errorf("verdict = %+v, want ask with an unjudged reason", got[0])
	}
}

func TestAFMMissingBinaryIsConfigError(t *testing.T) {
	_, err := afmBackend(t, filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("factory succeeded with a missing executable, want a ConfigError")
	}
	if _, ok := err.(*ConfigError); !ok {
		t.Fatalf("error type = %T (%v), want *ConfigError", err, err)
	}
}

func TestAFMHealth(t *testing.T) {
	factory, ok := backend.Lookup("afm")
	if !ok {
		t.Fatal("afm not registered")
	}
	healthy, err := factory(backend.Config{Values: map[string]string{afmExecEnv: filepath.Join("testdata", "fakefm.sh")}})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if err := healthy.HealthCheck(context.Background()); err != nil {
		t.Errorf("healthy HealthCheck = %v, want nil", err)
	}

	unready := script(t, "echo 'System model unavailable: modelNotReady'\nexit 1\n")
	b, err := afmBackend(t, unready)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	err = b.HealthCheck(context.Background())
	if err == nil || !strings.Contains(err.Error(), "modelNotReady") {
		t.Errorf("unready HealthCheck = %v, want modelNotReady", err)
	}
}
