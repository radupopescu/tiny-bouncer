package main_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file holds the response-cache contract tests over the built binary
// (architecture §5 step 2, plan T04): two identical mock check runs — first
// uncached, second fully cached with alignment preserved — plus --no-cache
// override, and the guarantee that raw command text never reaches the cache.

// cacheEnv returns an environment copy pointing the cache at a fresh test
// directory and clearing WISE_YOLO_CACHE so flag and env defaults do not
// fight.
func cacheEnv(t *testing.T) []string {
	t.Helper()
	root := t.TempDir()
	return append(scrubKeys(os.Environ()),
		"WISE_YOLO_CACHE_DIR="+root,
		"WISE_YOLO_CACHE=")
}

func runCheck(t *testing.T, env []string, args ...string) (contract, string) {
	t.Helper()
	_, stdout, stderr := runBinary(t, checkBin, `{"commands":["git status","rm -rf /"]}`,
		env, append([]string{"check", "--backend", "mock"}, args...)...)
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}
	return parse(t, stdout), stdout
}

func TestCheckBinaryCacheContract(t *testing.T) {
	env := cacheEnv(t)

	first, _ := runCheck(t, env)
	if first.Meta.Cached {
		t.Fatalf("first run meta = %+v; meta.cached must be false on a cold cache", first.Meta)
	}

	second, _ := runCheck(t, env)
	if !second.Meta.Cached {
		t.Fatalf("second run meta = %+v; meta.cached must be true", second.Meta)
	}
	if len(second.Results) != 2 ||
		second.Results[0].Command != "git status" || second.Results[0].Verdict != "allow" ||
		second.Results[1].Command != "rm -rf /" || second.Results[1].Verdict != "deny" {
		t.Fatalf("cached run results = %+v; both results must be served from cache, index-aligned", second.Results)
	}
	if second.Aggregate != first.Aggregate ||
		second.Results[0].Reason != first.Results[0].Reason ||
		second.Results[1].Reason != first.Results[1].Reason {
		t.Fatalf("cached run diverged from the cold run: %+v vs %+v", second, first)
	}

	third, _ := runCheck(t, env, "--no-cache")
	if third.Meta.Cached {
		t.Fatalf("--no-cache run meta = %+v; cached must be false", third.Meta)
	}
	if third.Aggregate != first.Aggregate {
		t.Fatalf("--no-cache run aggregate diverged: %+v vs %+v", third, first)
	}

	// --cache on an already-populated cache stays cached (flag override only
	// toggles the default, not the outcome).
	fourth, _ := runCheck(t, env, "--cache")
	if !fourth.Meta.Cached {
		t.Fatalf("--cache run meta = %+v; expected cache hits", fourth.Meta)
	}
}

func TestCheckBinaryCacheNeverStoresCommandText(t *testing.T) {
	env := cacheEnv(t)
	secret := "WISEYOLO-SEKRIT-e4b1f77c"

	_, _, stderr := runBinary(t, checkBin,
		`{"commands":["git status`+secret+`-probe"]}`,
		env, "check", "--backend", "mock")
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}

	root := ""
	for _, kv := range env {
		if s, ok := strings.CutPrefix(kv, "WISE_YOLO_CACHE_DIR="); ok {
			root = s
		}
	}
	if root == "" {
		t.Fatal("no cache dir env")
	}
	err := filepath.Walk(root, func(p string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(data), secret) {
			t.Errorf("secret command text leaked into %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the cache dir: %v", err)
	}
}
