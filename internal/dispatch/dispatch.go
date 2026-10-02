// Package dispatch orchestrates one check run: normalisation, the response
// cache hook, the backend Classify call, the filling of unjudged entries,
// aggregation, and construction of the output contract (architecture §3 and
// §5, pipeline steps 1–5).
package dispatch

import (
	"context"
	"regexp"
	"strings"
	"time"

	"wiseyolo/internal/backend"
	"wiseyolo/internal/core"
	"wiseyolo/internal/policy"
)

// MaxCommands is the largest batch accepted from a single check invocation
// (plan T03: reject larger inputs with exit 1).
const MaxCommands = 256

// FailureEffect is the effect applied to entries the backend could not judge
// (architecture §5 step 4): fail safe to ask. Plugin-configurability arrives
// with the plugin task (T11).
const FailureEffect = core.Ask

// whitespaceRuns matches every internal run of whitespace to be collapsed to
// one space while normalising (architecture §5 step 1).
var whitespaceRuns = regexp.MustCompile(`\s+`)

// Normalise trims a command and collapses internal whitespace runs to one
// space, preserving case (paths are case-sensitive). It exists for the cache
// key only; the backend always sees the raw command text.
func Normalise(cmd string) string {
	return whitespaceRuns.ReplaceAllString(strings.TrimSpace(cmd), " ")
}

// CacheHook is the response-cache extension point (architecture §5 step 2).
// NoCache is the inert hook (used by eval and when the cache is disabled);
// Cache is the disk-backed implementation (task T04).
type CacheHook interface {
	// Lookup reports a cached verdict for a normalised command, if any. The
	// version arguments join the cache key (architecture §5 step 2).
	Lookup(cmd, backendName, requestedModel, policyVersion, thresholdsVersion string) (core.Verdict, bool)
	// Store records the verdict under the same key.
	Store(cmd, backendName, requestedModel, policyVersion, thresholdsVersion string, v core.Verdict)
}

// NoCache is the inert hook: every lookup misses, every store is discarded
// (architecture §5 step 2 — eval forces this so measurements are genuine
// backend round trips).
type NoCache struct{}

// Lookup always misses.
func (NoCache) Lookup(string, string, string, string, string) (core.Verdict, bool) {
	return core.Verdict{}, false
}

// Store discards the verdict.
func (NoCache) Store(string, string, string, string, string, core.Verdict) {}

// Result is one output-contract row, index-aligned with the input commands.
type Result struct {
	Command    string   `json:"command"`
	Verdict    string   `json:"verdict"`
	Confidence float64  `json:"confidence"`
	Categories []string `json:"categories"`
	Reason     string   `json:"reason"`
}

// Aggregate is the single effect the plugin applies to the whole batch.
type Aggregate struct {
	Effect string `json:"effect"`
	Reason string `json:"reason"`
}

// Meta records the facts of one check invocation (architecture §3).
type Meta struct {
	Backend           string `json:"backend"`
	BackendModel      string `json:"backend_model"`
	PolicyVersion     string `json:"policy_version"`
	ThresholdsVersion string `json:"thresholds_version"`
	WallMS            int64  `json:"wall_ms"`
	Cached            bool   `json:"cached"`
	Attempts          string `json:"attempts"`
}

// Output is the complete check output contract.
type Output struct {
	Results   []Result  `json:"results"`
	Aggregate Aggregate `json:"aggregate"`
	Meta      Meta      `json:"meta"`
}

// Runner runs one classification batch against a single backend.
type Runner struct {
	backend backend.Backend
	cache   CacheHook
}

// New builds a Runner for a backend; a nil cache hook defaults to NoCache.
func New(b backend.Backend, cache CacheHook) *Runner {
	if cache == nil {
		cache = NoCache{}
	}
	return &Runner{backend: b, cache: cache}
}

// Run classifies the batch and assembles the output contract. Unjudged
// entries receive the failure effect with a reason naming the failure mode;
// wall_ms is measured around the classification only.
func (r *Runner) Run(ctx context.Context, commands []string) Output {
	start := time.Now()
	verdicts, cached := r.classify(ctx, commands)
	wall := time.Since(start)

	results := make([]Result, len(commands))
	for i, cmd := range commands {
		v := verdicts[i]
		results[i] = Result{
			Command:    cmd,
			Verdict:    string(v.Effect),
			Confidence: v.Confidence,
			Categories: ensureSlice(v.Categories),
			Reason:     v.Reason,
		}
	}

	agg := policy.Aggregate(verdicts)
	info := r.backend.Info()
	return Output{
		Results:   results,
		Aggregate: Aggregate{Effect: string(agg.Effect), Reason: agg.Reason},
		Meta: Meta{
			Backend:           info.Name,
			BackendModel:      info.Model,
			PolicyVersion:     info.PolicyVersion,
			ThresholdsVersion: info.ThresholdsVersion,
			WallMS:            wall.Milliseconds(),
			Cached:            cached,
			Attempts:          "1",
		},
	}
}

// classify resolves every command to a Verdict: cached entries are served
// from the cache hook (keyed on normalised text), the rest go to the backend
// in one Classify call; entries the backend fails to judge are filled with
// the failure verdict (architecture §5 steps 1–4).
func (r *Runner) classify(ctx context.Context, commands []string) ([]core.Verdict, bool) {
	verdicts := make([]core.Verdict, len(commands))
	cached := false
	info := r.backend.Info()

	var pending []int
	for i, cmd := range commands {
		normalised := Normalise(cmd)
		if v, ok := r.cache.Lookup(normalised, info.Name, info.Model, info.PolicyVersion, info.ThresholdsVersion); ok {
			verdicts[i] = v
			cached = true
			continue
		}
		pending = append(pending, i)
	}
	if len(pending) == 0 {
		return verdicts, cached
	}

	batch := make([]core.Command, len(pending))
	for j, i := range pending {
		batch[j] = core.Command{Raw: commands[i]}
	}
	got, err := r.backend.Classify(ctx, batch)
	for j, i := range pending {
		if err != nil {
			verdicts[i] = unjudged("backend error: " + err.Error())
			continue
		}
		if j >= len(got) {
			verdicts[i] = unjudged("backend returned no verdict for this command")
			continue
		}
		if !judged(got[j]) {
			verdicts[i] = unjudged("backend returned an unusable verdict for this command")
			continue
		}
		verdicts[i] = got[j]
		normalised := Normalise(commands[i])
		r.cache.Store(normalised, info.Name, info.Model, info.PolicyVersion, info.ThresholdsVersion, got[j])
	}
	return verdicts, cached
}

// judged reports whether the backend produced a usable verdict for an entry:
// one whose effect is one of the three defined effects.
func judged(v core.Verdict) bool {
	switch v.Effect {
	case core.Allow, core.Deny, core.Ask:
		return true
	}
	return false
}

// unjudged builds the failure verdict for an entry the backend could not
// judge: the configured failure effect with a reason naming the failure mode
// (architecture §5 step 4).
func unjudged(failureMode string) core.Verdict {
	return core.Verdict{
		Effect:     FailureEffect,
		Confidence: 0,
		Reason:     "unjudged: " + failureMode,
	}
}

// ensureSlice returns a non-nil slice so empty categories marshal as []
// rather than null.
func ensureSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
