// Eval runs (architecture §3 eval, §7): a corpus record is pushed through the
// normal one-command pipeline so per-record wall_ms measures real latency, and
// the cache is forced off so every measurement is a genuine backend round
// trip. The spawn benchmark separately measures empty-input process cost.
package eval

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"time"

	"tinybouncer/internal/backend"
	"tinybouncer/internal/core"
	"tinybouncer/internal/dispatch"
)

// emptyInput is the stdin payload for the spawn benchmark: an empty batch.
var emptyInput = []byte(`{"commands": []}`)

// RunBackend classifies every record of the corpus through the dispatch
// pipeline (one command per invocation, cache forced off) and returns the
// scored records in corpus order plus the summed token usage reported by the
// backend (zero for backends without usage reporting). The runner fills
// unjudged entries with the failure verdict exactly as `check` would, so
// eval measures the deployed behaviour, not an idealised one.
func RunBackend(ctx context.Context, b backend.Backend, set *Set) ([]Scored, Usage, error) {
	runner := dispatch.New(b, dispatch.NoCache{})
	out := make([]Scored, len(set.Records))
	var usage Usage
	tracker, tracksUsage := b.(backend.UsageTracker)
	for i, r := range set.Records {
		res := runner.Run(ctx, []string{r.Command})
		if len(res.Results) != 1 {
			return nil, usage, &RunError{Message: "pipeline broke index alignment at corpus record " +
				strconv.Itoa(i) + ": results length " + strconv.Itoa(len(res.Results))}
		}
		if tracksUsage {
			u := tracker.LastUsage()
			usage.Requests += u.Requests
			usage.InputTokens += u.InputTokens
			usage.OutputTokens += u.OutputTokens
		}
		out[i] = Scored{
			Record:     r,
			Verdict:    core.Effect(res.Results[0].Verdict),
			Reason:     res.Results[0].Reason,
			Categories: res.Results[0].Categories,
			WallMS:     res.Meta.WallMS,
		}
	}
	return out, usage, nil
}

// RunError is an eval-run failure.
type RunError struct{ Message string }

func (e *RunError) Error() string { return "eval run: " + e.Message }

// Bench is the spawn-overhead benchmark result (architecture §7 latency):
// empty-input invocations of the running binary.
type Bench struct {
	Runs   int     `json:"runs"`
	MeanMS float64 `json:"mean_ms"`
	P95MS  float64 `json:"p95_ms"`
}

// MinBenchRuns is the smallest sample population accepted for the benchmark.
const MinBenchRuns = 20

// BenchSpawn times MinBenchRuns invocations of the running binary (resolved
// with os.Executable) performing an empty `check` classification, reporting
// mean and p95 wall-clock milliseconds. Caches are disabled in the
// subprocess so the number isolates process startup plus empty-batch cost.
func BenchSpawn(ctx context.Context, backendFlag string) (Bench, error) {
	self, err := os.Executable()
	if err != nil {
		return Bench{}, &RunError{Message: "resolve current binary: " + err.Error()}
	}
	args := []string{"check", "--backend", backendFlag, "--no-cache"}
	var durations []time.Duration
	for i := 0; i < MinBenchRuns; i++ {
		start := time.Now()
		cmd := exec.CommandContext(ctx, self, args...)
		cmd.Stdin = bytes.NewReader(emptyInput)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return Bench{}, &RunError{Message: "bench spawn run " + strconv.Itoa(i) + " failed: " +
				err.Error() + ": " + string(out)}
		}
		durations = append(durations, time.Since(start))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	var mean time.Duration
	for _, d := range durations {
		mean += d
	}
	n := len(durations)
	return Bench{
		Runs:   n,
		MeanMS: float64(mean.Microseconds()) / 1000 / float64(n),
		P95MS:  float64(durations[nearestRank(0.95, n)-1].Microseconds()) / 1000,
	}, nil
}
