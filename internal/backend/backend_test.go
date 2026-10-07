package backend

import (
	"context"
	"testing"

	"tinybouncer/internal/core"
)

// stub satisfies the full Backend interface; keeping it here forces a
// compile-time check whenever the interface changes (task T03 amendment:
// Info is part of the interface).
type stub struct{}

func (stub) Name() string                                                     { return "stub" }
func (stub) Info() Info                                                       { return Info{Name: "stub"} }
func (stub) HealthCheck(context.Context) error                                { return nil }
func (stub) Classify(context.Context, []core.Command) ([]core.Verdict, error) { return nil, nil }

var _ Backend = stub{}

func TestRegistry(t *testing.T) {
	f1 := func(Config) (Backend, error) { return nil, nil }
	Register("t01-a", f1)
	if f, ok := Lookup("t01-a"); !ok || f == nil {
		t.Fatal("Lookup(t01-a) missing")
	}
	if _, ok := Lookup("nope"); ok {
		t.Fatal("Lookup(nope) should miss")
	}
	names := Names()
	found := false
	for _, n := range names {
		if n == "t01-a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Names() = %v, does not include t01-a", names)
	}
	Register("t01-b", f1)
	names = Names()
	if i := indexOf(names, "t01-a"); i >= 0 && indexOf(names, "t01-b") < i {
		t.Fatalf("Names() not sorted: %v", names)
	}
}

func TestEnv(t *testing.T) {
	t.Setenv("TINY_BOUNCER_TEST_KEY", "v")
	if got := Env("TINY_BOUNCER_TEST_KEY", "d"); got != "v" {
		t.Fatalf("Env = %q, want v", got)
	}
	if got := Env("TINY_BOUNCER_TEST_MISSING", "d"); got != "d" {
		t.Fatalf("Env = %q, want d", got)
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}
