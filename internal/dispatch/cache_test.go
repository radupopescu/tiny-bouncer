package dispatch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tinybouncer/internal/backend"
	_ "tinybouncer/internal/backend/mock" // register the offline backend for the runner test
	"tinybouncer/internal/core"
)

// withCacheRoot points TINY_BOUNCER_CACHE_DIR at a fresh temporary directory for
// one test (the test-only override documented on cacheDirEnv).
func withCacheRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("TINY_BOUNCER_CACHE_DIR", root)
	return root
}

func TestCacheKeyDerivationChangesWithEveryInput(t *testing.T) {
	base := cacheKey("git status", "mock", "mock-rules", "mock-0", "mock-0")
	cases := []struct {
		name string
		key  string
	}{
		{"changed command", cacheKey("git status ", "mock", "mock-rules", "mock-0", "mock-0")},
		{"changed backend", cacheKey("git status", "jev", "mock-rules", "mock-0", "mock-0")},
		{"changed model", cacheKey("git status", "mock", "jev-latest", "mock-0", "mock-0")},
		{"changed policy version", cacheKey("git status", "mock", "mock-rules", "mock-1", "mock-0")},
		{"changed thresholds version", cacheKey("git status", "mock", "mock-rules", "mock-0", "tv1")},
	}
	for _, tc := range cases {
		if tc.key == base {
			t.Errorf("%s: key unchanged, want a different key", tc.name)
		}
	}
	if base != cacheKey("git status", "mock", "mock-rules", "mock-0", "mock-0") {
		t.Error("identical inputs must derive the same key")
	}
	if base != cacheKey(Normalise(" git\tstatus"), "mock", "mock-rules", "mock-0", "mock-0") {
		t.Error("normalised-equivalent commands must share a key")
	}
}

const testBackend = "mock"
const testModel = "mock-rules"

func storeVerdict(t *testing.T, c *Cache, cmd string) core.Verdict {
	return storeCustom(t, c, testBackend, testModel, core.Verdict{
		Effect: core.Allow, Confidence: 0.97, Categories: []string{"vcs_read"},
		Reason: "read-only inspection",
	}, cmd)
}

func storeCustom(t *testing.T, c *Cache, be, model string, v core.Verdict, cmd string) core.Verdict {
	t.Helper()
	v.Categories = ensureSlice(v.Categories)
	c.Store(cmd, be, model, "mock-0", "mock-0", v)
	return v
}

// sameVerdict compares two verdicts field by field (Verdict holds a slice, so
// it is not comparable with ==).
func sameVerdict(a, b core.Verdict) bool {
	if len(a.Categories) != len(b.Categories) {
		return false
	}
	for i := range a.Categories {
		if a.Categories[i] != b.Categories[i] {
			return false
		}
	}
	return a.Effect == b.Effect && a.Confidence == b.Confidence && a.Reason == b.Reason
}

func TestCacheRoundTrip(t *testing.T) {
	withCacheRoot(t)
	c := NewCache()
	want := storeVerdict(t, c, "git status")
	got, ok := c.Lookup("git status", testBackend, testModel, "mock-0", "mock-0")
	if !ok {
		t.Fatal("lookup missed immediately after a store")
	}
	if !sameVerdict(got, want) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, want)
	}
	// A different key space must not serve the entry.
	if _, ok := c.Lookup("git status", "jev", testModel, "mock-0", "mock-0"); ok {
		t.Error("entry served under a different backend")
	}
	if _, ok := c.Lookup("rm -rf /", testBackend, testModel, "mock-0", "mock-0"); ok {
		t.Error("unrelated command matched")
	}
}

func TestCacheCorruptionTolerance(t *testing.T) {
	withCacheRoot(t)
	c := NewCache()
	storeVerdict(t, c, "git status")

	p := c.path("git status", testBackend, testModel, "mock-0", "mock-0")
	for _, tc := range []string{"", "{", "{\"effect\":"} {
		if err := os.WriteFile(p, []byte(tc), 0o600); err != nil {
			t.Fatalf("writing corrupt payload: %v", err)
		}
		if _, ok := c.Lookup("git status", testBackend, testModel, "mock-0", "mock-0"); ok {
			t.Errorf("corrupt entry %q served", tc)
		}
	}

	// After corruption a fresh store must overwrite and serve again.
	want := storeVerdict(t, c, "git status")
	got, ok := c.Lookup("git status", testBackend, testModel, "mock-0", "mock-0")
	if !ok || !sameVerdict(got, want) {
		t.Fatalf("post-corruption overwrite failed: ok=%v got=%+v", ok, got)
	}

	// A unreadable (permission-denied) file is also just a miss.
	fpath := c.path("git status", testBackend, testModel, "mock-0", "mock-0")
	if err := os.Chmod(fpath, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(fpath, 0o600)
	if _, ok := c.Lookup("git status", testBackend, testModel, "mock-0", "mock-0"); ok {
		t.Error("unreadable entry served")
	}
}

// dirOf is a convenience for the parent directory of a path.
func dirOf(p string) string {
	return filepath.Dir(p)
}

func TestCacheTTLScan(t *testing.T) {
	withCacheRoot(t)
	c := NewCache()
	storeVerdict(t, c, "git status")

	// Age the entry past the TTL on disk, then run the purge scan directly.
	p := c.path("git status", testBackend, testModel, "mock-0", "mock-0")
	old := time.Now().Add(-31 * 24 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	root := os.Getenv("TINY_BOUNCER_CACHE_DIR")
	purge(root)
	if _, err := os.Stat(p); err == nil {
		t.Fatal("purge left an over-TTL entry in place")
	}

	// A fresh entry survives the scan.
	storeVerdict(t, c, "git log")
	root = os.Getenv("TINY_BOUNCER_CACHE_DIR")
	purge(root)
	if _, ok := c.Lookup("git log", testBackend, testModel, "mock-0", "mock-0"); !ok {
		t.Error("purge removed a fresh entry")
	}

	// A missing cache directory is not an error to the scan.
	purge(filepath.Join(t.TempDir(), "absent-if-this-appears"))
}

func TestCacheEntryFileMode(t *testing.T) {
	withCacheRoot(t)
	c := NewCache()
	storeVerdict(t, c, "git status")
	info, err := os.Stat(c.path("git status", testBackend, testModel, "mock-0", "mock-0"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("entry mode = %o, want 0600", got)
	}
	dir := dirOf(c.path("git status", testBackend, testModel, "mock-0", "mock-0"))
	dinfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dinfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("directory mode = %o, want 0700", got)
	}
}

func TestCacheNoCommandTextPersisted(t *testing.T) {
	root := withCacheRoot(t)
	c := NewCache()
	secret := "SEKRIT-DEPLOY-TOKEN-3f8a"
	c.Store(secret, "mock", "mock-rules", "mock-0", "mock-0",
		core.Verdict{Effect: core.Ask, Confidence: 0.5, Categories: []string{}, Reason: "no rule matched"})
	if _, ok := c.Lookup(secret, "mock", "mock-rules", "mock-0", "mock-0"); !ok {
		t.Fatal("cache missed straight after store")
	}
	// Grep the whole cache dir for the secret: nothing may match.
	var found []string
	root = os.Getenv("TINY_BOUNCER_CACHE_DIR")
	if err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), secret) {
			found = append(found, p)
		}
		return nil
	}); err != nil {
		t.Fatalf("walking cache dir: %v", err)
	}
	if len(found) > 0 {
		t.Fatalf("secret leaked into cache files: %v", found)
	}
}

func TestCacheDisabledBehavesAsNoCache(t *testing.T) {
	c := new(Cache) // root "" → disabled
	c.Store("git status", testBackend, testModel, "mock-0", "mock-0",
		core.Verdict{Effect: core.Allow, Confidence: 1, Reason: "safe"})
	if _, ok := c.Lookup("git status", testBackend, testModel, "mock-0", "mock-0"); ok {
		t.Error("disabled cache served an entry")
	}
}

func TestDefaultCacheEnabledEnv(t *testing.T) {
	for _, on := range []string{"", "1", "true", "on", "yes"} {
		t.Setenv("TINY_BOUNCER_CACHE", on)
		if !DefaultCacheEnabled() {
			t.Errorf("TINY_BOUNCER_CACHE=%q: default reported off, want on", on)
		}
	}
	for _, off := range []string{"0", "false", "off", "no"} {
		t.Setenv("TINY_BOUNCER_CACHE", off)
		if DefaultCacheEnabled() {
			t.Errorf("TINY_BOUNCER_CACHE=%q: default reported on, want off", off)
		}
	}
}

// TestCacheFilesAreOnlyExpectedSchema asserts the on-disk shape stays the
// documented minimal JSON: verdict fields, debug notes, timestamps — no
// command text at all.
func TestCacheFilesContainOnlyVerdictFields(t *testing.T) {
	withCacheRoot(t)
	c := NewCache()
	storeVerdict(t, c, "git status -q")
	p := c.path("git status -q", testBackend, testModel, "mock-0", "mock-0")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("entry is not JSON: %v", err)
	}
	for _, k := range []string{"effect", "confidence", "categories", "reason", "model", "created", "key"} {
		if _, ok := m[k]; !ok {
			t.Errorf("entry missing field %q", k)
		}
	}
	if _, ok := m["command"]; ok {
		t.Error("entry must not carry a command field")
	}
}

// viaHookRun exercises the cache through the Runner like `check` does. It
// uses the real mock backend; no network is involved anywhere.
func TestRunnerServesFromDiskCache(t *testing.T) {
	withCacheRoot(t)
	out1 := New(lookupMock(t), NewCache()).Run(context.Background(), []string{"git status", "git log"})
	if out1.Meta.Cached {
		t.Error("first run reported cached=true, want false")
	}
	out2 := New(lookupMock(t), NewCache()).Run(context.Background(), []string{"git status", "git log"})
	if !out2.Meta.Cached {
		t.Error("second run reported cached=false, want true")
	}
	if out1.Aggregate != out2.Aggregate || !sameResults(out1.Results, out2.Results) {
		t.Fatalf("cached run diverged:\n%+v\nvs\n%+v", out1, out2)
	}
}

// sameResults asserts two result tables carry identical rows, order included.
func sameResults(a, b []Result) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Command != b[i].Command || a[i].Verdict != b[i].Verdict ||
			a[i].Confidence != b[i].Confidence || a[i].Reason != b[i].Reason {
			return false
		}
		if len(a[i].Categories) != len(b[i].Categories) {
			return false
		}
		for j := range a[i].Categories {
			if a[i].Categories[j] != b[i].Categories[j] {
				return false
			}
		}
	}
	return true
}

func lookupMock(t *testing.T) backend.Backend {
	t.Helper()
	f, ok := backend.Lookup(testBackend)
	if !ok {
		t.Fatal("mock backend not registered")
	}
	b, err := f(backend.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
