// Package backend defines the narrow abstraction behind which all judgment
// backends live. Backends never leak into the CLI contract, the eval harness,
// or the OpenCode plugin.
//
// Each concrete backend lives in its own package under internal/backend/<name>
// and self-registers via init() + Register. Every backend package must fulfil
// three obligations (architecture §5.2):
//
//  1. Policy blob: its own judgment payload (state shape and questions, or a
//     prompt schema), versioned as PolicyVersion; thresholds live in code and
//     carry a separate ThresholdsVersion.
//  2. Verdict mapping: native output mapped to the generic core.Verdict, with
//     its own certainty discipline applied (a deny only when the backend's
//     confidence holds; nothing auto-upgrades to allow).
//  3. HealthCheck: credential and reachability verification feeding doctor.
package backend

import (
	"context"
	"os"
	"sort"
	"sync"

	"wiseyolo/internal/core"
)

// Backend is one judgment provider. Classify must return one Verdict per
// input, index-aligned; it must not reorder, drop, or merge inputs, and it
// must fill entries it could not judge with a failure verdict.
type Backend interface {
	// Name is the stable id used in reports, logs, and cache keys.
	Name() string
	// Info returns the identity and version facts recorded in the check
	// output contract's meta object (architecture §3). Amendment (T03): the
	// contract requires backend_model, policy_version and thresholds_version
	// facts from the selected backend, so Info is part of the interface.
	Info() Info
	// Classify judges a batch of commands in one call.
	Classify(ctx context.Context, cmds []core.Command) ([]core.Verdict, error)
	// HealthCheck verifies credentials and reachability (feeds doctor).
	HealthCheck(ctx context.Context) error
}

// Info carries the identity and version facts recorded in meta, reports, and
// history.jsonl.
type Info struct {
	Name, Model, PolicyVersion, ThresholdsVersion string
}

// Usage reports the token consumption of the most recent Classify call
// (architecture §5, step 5 and §5bis: usage is recorded in eval reports).
type Usage struct {
	Requests     int `json:"requests"`
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// UsageTracker is an optional Backend extension for backends that report
// token usage. The eval harness type-asserts to it and sums the reported
// usage across the corpus run; callers must treat it as best-effort.
type UsageTracker interface {
	// LastUsage returns the usage of the most recent Classify call (zero
	// before any call).
	LastUsage() Usage
}

// Config carries backend-specific configuration values, populated from
// environment variables named by each backend package (e.g. WISE_YOLO_JEV_*).
type Config struct {
	Values map[string]string
}

// Factory builds a Backend from configuration, returning a typed config error
// when required configuration is missing or invalid.
type Factory func(cfg Config) (Backend, error)

// Env looks up key in the process environment and returns fallback when it is
// unset or empty. Backend factories use this helper (or Config.Values) rather
// than reading os directly, so configuration stays testable.
func Env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

var registry = struct {
	sync.RWMutex
	factories map[string]Factory
}{factories: make(map[string]Factory)}

// Register installs a backend factory under a stable id. Intended to be called
// from backend packages' init().
func Register(name string, f Factory) {
	registry.Lock()
	defer registry.Unlock()
	registry.factories[name] = f
}

// Lookup returns the factory registered under name, if any.
func Lookup(name string) (Factory, bool) {
	registry.RLock()
	defer registry.RUnlock()
	f, ok := registry.factories[name]
	return f, ok
}

// Names returns the registered backend ids in sorted order.
func Names() []string {
	registry.RLock()
	defer registry.RUnlock()
	names := make([]string, 0, len(registry.factories))
	for n := range registry.factories {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
