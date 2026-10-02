package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"wiseyolo/internal/backend"
	"wiseyolo/internal/core"
)

// stubBackend is a scripted in-process backend for pipeline tests.
type stubBackend struct {
	info     backend.Info
	classify func(ctx context.Context, cmds []core.Command) ([]core.Verdict, error)
	classes  [][]core.Command // raw batches as seen by Classify
}

func (s *stubBackend) Name() string                      { return s.info.Name }
func (s *stubBackend) Info() backend.Info                { return s.info }
func (s *stubBackend) HealthCheck(context.Context) error { return nil }
func (s *stubBackend) Classify(ctx context.Context, cmds []core.Command) ([]core.Verdict, error) {
	s.classes = append(s.classes, cmds)
	return s.classify(ctx, cmds)
}

var _ backend.Backend = (*stubBackend)(nil)

func TestNormalise(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"  git status  ", "git status"},
		{"git  status", "git status"},
		{"echo\t a \t b", "echo a b"},
		{"echo \"rm \t -rf\t/\"", "echo \"rm -rf /\""},
		{"Git STATUS", "Git STATUS"}, // case is preserved
		{"cat ./CamelCase/File", "cat ./CamelCase/File"},
	}
	for _, c := range cases {
		if got := Normalise(c.in); got != c.want {
			t.Errorf("Normalise(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRunnerHappyPath(t *testing.T) {
	b := &stubBackend{info: backend.Info{Name: "stub", Model: "m", PolicyVersion: "p", ThresholdsVersion: "tv"}}
	b.classify = func(_ context.Context, cmds []core.Command) ([]core.Verdict, error) {
		out := make([]core.Verdict, len(cmds))
		for i, c := range cmds {
			effect := core.Allow
			if c.Raw == "rm -rf /" {
				effect = core.Deny
			}
			out[i] = core.Verdict{Effect: effect, Confidence: 1.0, Categories: []string{"c"}, Reason: "r " + c.Raw}
		}
		return out, nil
	}
	out := New(b, nil).Run(context.Background(), []string{"git status", "rm -rf /"})
	if len(out.Results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(out.Results))
	}
	if out.Results[0].Command != "git status" || out.Results[1].Command != "rm -rf /" {
		t.Fatalf("results not index-aligned with input: %+v", out.Results)
	}
	if out.Results[0].Verdict != "allow" || out.Results[1].Verdict != "deny" {
		t.Fatalf("verdicts: %+v", out.Results)
	}
	// The backend must see the raw text, not the normalised form.
	if got := b.classes[0][0].Raw; got != "git status" {
		t.Fatalf("Classify saw %q; backend must receive the raw command", got)
	}
	if len(b.classes) != 1 || len(b.classes[0]) != 2 {
		t.Fatalf("Classify batches: %+v", b.classes)
	}
	if out.Aggregate.Effect != "deny" {
		t.Fatalf("aggregate = %q, want deny", out.Aggregate.Effect)
	}
	if out.Aggregate.Reason != "r rm -rf /" {
		t.Fatalf("aggregate reason = %q", out.Aggregate.Reason)
	}
	m := out.Meta
	if m.Backend != "stub" || m.BackendModel != "m" || m.PolicyVersion != "p" || m.ThresholdsVersion != "tv" {
		t.Fatalf("meta backend facts: %+v", m)
	}
	if m.WallMS < 0 {
		t.Fatalf("wall_ms = %d, want >= 0", m.WallMS)
	}
	if m.Cached {
		t.Fatal("cached = true before T04, want false")
	}
	if m.Attempts != "1" {
		t.Fatalf("attempts = %q, want 1", m.Attempts)
	}
}

func TestRunnerRawCommandsPreserved(t *testing.T) {
	b := &stubBackend{info: backend.Info{Name: "stub"}}
	b.classify = func(_ context.Context, cmds []core.Command) ([]core.Verdict, error) {
		return make([]core.Verdict, len(cmds)), nil
	}
	out := New(b, nil).Run(context.Background(), []string{"  echo \t WEIRD \n case "})
	if got := b.classes[0][0].Raw; got != "  echo \t WEIRD \n case " {
		t.Fatalf("Classify saw normalised text %q; backend must receive the raw command", got)
	}
	for _, r := range out.Results {
		if r.Categories == nil {
			t.Fatal("categories nil for a zero verdict; want []")
		}
	}
}

func TestRunnerClassifyErrorFillsUnjudged(t *testing.T) {
	b := &stubBackend{info: backend.Info{Name: "stub", Model: "m", PolicyVersion: "p", ThresholdsVersion: "tv"}}
	b.classify = func(context.Context, []core.Command) ([]core.Verdict, error) {
		return nil, errors.New("connection refused (test)")
	}
	out := New(b, nil).Run(context.Background(), []string{"echo hi", "rm -rf /"})
	for i, r := range out.Results {
		if r.Verdict != "ask" {
			t.Errorf("result %d verdict = %q, want ask (failure effect)", i, r.Verdict)
		}
		if !strings.Contains(r.Reason, "connection refused") {
			t.Errorf("result %d reason %q does not name the failure mode", i, r.Reason)
		}
	}
	if out.Aggregate.Effect != "ask" {
		t.Fatalf("aggregate = %q, want ask", out.Aggregate.Effect)
	}
	if out.Aggregate.Reason == "" {
		t.Fatal("aggregate reason empty; want the failure reason")
	}
}

func TestRunnerPartialResultsUnjudged(t *testing.T) {
	b := &stubBackend{info: backend.Info{Name: "stub"}}
	b.classify = func(_ context.Context, cmds []core.Command) ([]core.Verdict, error) {
		return []core.Verdict{{Effect: core.Allow, Categories: []string{"c"}}}, nil
	}
	out := New(b, nil).Run(context.Background(), []string{"a", "b", "c"})
	if out.Results[0].Verdict != "allow" {
		t.Errorf("result 0 = %q, want allow", out.Results[0].Verdict)
	}
	for i := 1; i < 3; i++ {
		if out.Results[i].Verdict != "ask" {
			t.Errorf("result %d = %q, want ask (missing entry)", i, out.Results[i].Verdict)
		}
	}
}

func TestRunnerUnusableEffectUnjudged(t *testing.T) {
	b := &stubBackend{info: backend.Info{Name: "stub"}}
	b.classify = func(context.Context, []core.Command) ([]core.Verdict, error) {
		return []core.Verdict{{Effect: "maybe"}}, nil
	}
	out := New(b, nil).Run(context.Background(), []string{"a"})
	if out.Results[0].Verdict != "ask" || out.Results[0].Reason == "" {
		t.Fatalf("result = %+v, want ask with failure reason", out.Results[0])
	}
}

func TestRunnerEmptyBatch(t *testing.T) {
	b := &stubBackend{info: backend.Info{Name: "stub", Model: "m"}}
	b.classify = func(context.Context, []core.Command) ([]core.Verdict, error) {
		t.Error("Classify called for an empty batch; should short-circuit")
		return nil, fmt.Errorf("must not be called")
	}
	out := New(b, nil).Run(context.Background(), nil)
	if len(out.Results) != 0 {
		t.Fatalf("results = %+v, want empty", out.Results)
	}
	if out.Aggregate.Effect != "allow" {
		t.Fatalf("empty-batch aggregate = %q, want allow", out.Aggregate.Effect)
	}
	if out.Meta.Cached {
		t.Fatal("cached = true for an empty batch")
	}
}

// recordingCache records the keys and verdicts moving through the hook.
type recordingCache struct {
	lookups []string
	stores  []string
	serve   map[string]core.Verdict
}

func (c *recordingCache) Lookup(cmd string, _ string, _ string, _ string, _ string) (core.Verdict, bool) {
	c.lookups = append(c.lookups, cmd)
	v, ok := c.serve[cmd]
	return v, ok
}

func (c *recordingCache) Store(cmd string, _ string, _ string, _ string, _ string, _ core.Verdict) {
	c.stores = append(c.stores, cmd)
}

func TestRunnerCacheHookKeysNormalised(t *testing.T) {
	b := &stubBackend{info: backend.Info{Name: "stub", Model: "m", PolicyVersion: "p", ThresholdsVersion: "tv"}}
	b.classify = func(_ context.Context, cmds []core.Command) ([]core.Verdict, error) {
		out := make([]core.Verdict, len(cmds))
		for i := range cmds {
			out[i] = core.Verdict{Effect: core.Allow, Categories: []string{"c"}}
		}
		return out, nil
	}
	c := &recordingCache{serve: map[string]core.Verdict{"git status": {Effect: core.Ask}}}
	out := New(b, c).Run(context.Background(), []string{"  git  status  ", "echo hi"})
	// Lookup used the normalised key (case preserved) and served the verdict.
	if len(c.lookups) != 2 || c.lookups[0] != "git status" {
		t.Fatalf("lookups = %v", c.lookups)
	}
	if got := out.Results[0].Verdict; got != "ask" {
		t.Fatalf("result 0 = %q, want the cache-served ask", got)
	}
	if !out.Meta.Cached {
		t.Fatal("meta.cached = false with a cache hit, want true")
	}
	// Stored under the same normalised key.
	if len(c.stores) != 1 || c.stores[0] != "echo hi" {
		t.Fatalf("stores = %v", c.stores)
	}
}
