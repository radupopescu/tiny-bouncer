// The eval subcommand (architecture §3 eval, §7 metrics/gates): run the
// labelled corpus through a backend, write a report, append one history
// line, optionally compare against the previous same-backend run under the
// regression gates, and optionally sweep threshold variants. Caching is
// forced off for the whole run so latencies are genuine round trips.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tinybouncer/internal/backend"
	"tinybouncer/internal/eval"
)

// gatesExit is the exit code for a run that violates an active gate
// (architecture §7: `eval --compare` fails non-zero on a gate breach). It
// stays distinct from 1 (usage/config) and 2 (internal error) so CI can tell
// a failing calibration from a broken invocation.
const gatesExit = 3

// thresholdsEnv is the jev sweep-override variable (T06): each --sweep
// variant is one full value of this variable for the corpus run.
const thresholdsEnv = "TINY_BOUNCER_JEV_THRESHOLDS"

// factoryFor resolves a backend factory or fails with a diagnostic.
func factoryFor(name string) (backend.Backend, error) {
	factory, ok := backend.Lookup(name)
	if !ok {
		return nil, fmt.Errorf("unknown backend %q (available: %s)", name, availableBackends())
	}
	return factory(backend.Config{})
}

// fileExists reports whether path is stat-able. A path that cannot be read
// because of a permission problem is reported absent here and surfaces as a
// load error only for an explicit --gates path.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// runEval implements the eval subcommand.
func runEval(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	backendName := fs.String("backend", backend.Env("TINY_BOUNCER_BACKEND", defaultBackend),
		"judgment backend id (registry name)")
	corpusFlag := fs.String("corpus", "", "evalset.json location (default: data/evalset.json found upwards from the working directory)")
	reportsFlag := fs.String("reports", "reports", "report and history directory")
	gatesFlag := fs.String("gates", "", "gates file to apply (with --compare, default <reports>/gates.json)")
	compare := fs.Bool("compare", false, "compare against the most recent same-backend history line")
	against := fs.String("against", "", "compare this run against the most recent report of another backend")
	sweep := fs.String("sweep", "", "semicolon-separated TINY_BOUNCER_JEV_THRESHOLDS variants (each comma-separated key=value pairs; jev only)")
	bench := fs.Bool("bench-spawn", false, "also benchmark empty-input spawns of this binary (mean and p95)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "tinybouncer eval: unexpected argument %q\n", fs.Arg(0))
		return 1
	}

	b, err := factoryFor(*backendName)
	if err != nil {
		fmt.Fprintf(stderr, "tinybouncer eval: backend %q: %v\n", *backendName, err)
		return 1
	}
	info := b.Info()

	if *sweep != "" {
		return runSweep(b, info, *corpusFlag, *reportsFlag, *sweep, stdout, stderr)
	}

	set, err := corpusLoader(*corpusFlag)
	if err != nil {
		fmt.Fprintf(stderr, "tinybouncer eval: %v\n", err)
		return 1
	}

	scores, usage, err := eval.RunBackend(context.Background(), b, set)
	if err != nil {
		fmt.Fprintf(stderr, "tinybouncer eval: internal error: %v\n", err)
		return 2
	}

	var benchPtr *eval.Bench
	if *bench {
		res, err := eval.BenchSpawn(context.Background(), *backendName)
		if err != nil {
			fmt.Fprintf(stderr, "tinybouncer eval: %v\n", err)
			return 2
		}
		fmt.Fprintf(stdout, "spawn bench: runs=%d mean=%.2fms p95=%.2fms\n",
			res.Runs, res.MeanMS, res.P95MS)
		benchPtr = &res
	}

	rep := eval.BuildReport(time.Now(), info, set, scores, benchPtr, usage)
	current := rep.History()

	// The previous same-backend line is snapshotted before this run's own
	// line is appended, so --compare never compares with itself.
	lines, histErr := eval.LoadHistory(*reportsFlag)
	prev, hasPrev := eval.LatestSameBackend(lines, info.Name)

	path, err := rep.Write(*reportsFlag)
	if err != nil {
		fmt.Fprintf(stderr, "tinybouncer eval: write report: %v\n", err)
		return 2
	}
	if histErr != nil {
		fmt.Fprintf(stderr, "tinybouncer eval: history %q: %v\n", *reportsFlag, histErr)
		return 2
	}
	if err := eval.AppendHistory(*reportsFlag, current); err != nil {
		fmt.Fprintf(stderr, "tinybouncer eval: history %q: %v\n", *reportsFlag, err)
		return 2
	}

	fmt.Fprintf(stdout, "report: %s\n", path)
	fmt.Fprintf(stdout,
		"metrics: tp=%d fn=%d fp=%d tn=%d sensitivity=%.3f specificity=%.3f precision=%.3f f1=%.3f fnr=%.3f fpr=%.3f accuracy3=%.3f lat_p50=%.0fms lat_p95=%.0fms\n",
		rep.TruePositive, rep.FalseNegative, rep.FalsePositive, rep.TrueNegative,
		rep.Safety.Sensitivity, rep.Safety.Specificity, rep.Safety.Precision,
		rep.Safety.F1, rep.Safety.FNR, rep.Safety.FPR, rep.ThreeWay.Accuracy,
		rep.Latency.P50, rep.Latency.P95)

	if *against == "" && !*compare && *gatesFlag == "" {
		return 0
	}

	// Same-backend history deltas (T09 --compare). Preserved for a
	// gates-only invocation so its diagnostic output is unchanged.
	if *compare || (*against == "" && *gatesFlag != "") {
		if hasPrev {
			fmt.Fprintf(stdout, "compare: vs %s (thresholds %s)\n", prev.TS, prev.ThresholdsVersion)
			for _, d := range eval.Deltas(prev, current) {
				fmt.Fprintf(stdout, "  %s: %.4f → %.4f (Δ %+.4f)\n",
					d.Field, d.Previous, d.Current, d.Change)
			}
		} else {
			fmt.Fprintf(stdout, "compare: no previous %q line in history — nothing to compare yet\n",
				info.Name)
		}
	}

	// Cross-backend comparison (T15 --against): load the most recent report
	// for the named backend and break this run's agreement down against it. A
	// missing report is a note, not an error.
	if *against != "" {
		other, otherFound, err := eval.LatestReport(*reportsFlag, *against)
		if err != nil {
			fmt.Fprintf(stderr, "tinybouncer eval: against %q: %v\n", *against, err)
			return 2
		}
		if !otherFound {
			fmt.Fprintf(stdout, "against: no report found for backend %q in %s — nothing to compare\n",
				*against, *reportsFlag)
			return 0
		}
		cmp := eval.Compare(rep, other)
		fmt.Fprint(stdout, cmp.Table())
		cpath, err := cmp.Write(*reportsFlag)
		if err != nil {
			fmt.Fprintf(stderr, "tinybouncer eval: write comparison report: %v\n", err)
			return 2
		}
		fmt.Fprintf(stdout, "compare report: %s\n", cpath)
	}

	// Gates: an explicit --gates file is mandatory (must exist). With
	// --compare or --against the per-backend default
	// <reports>/gates-<backend>.json is preferred, falling back to the shared
	// <reports>/gates.json (kept for jev); absent defaults mean no gates are
	// applied and the exit stays 0.
	gatesPath := *gatesFlag
	if gatesPath == "" {
		specific := filepath.Join(*reportsFlag, eval.GatesName(info.Name))
		shared := filepath.Join(*reportsFlag, "gates.json")
		switch {
		case fileExists(specific):
			gatesPath = specific
		case fileExists(shared):
			gatesPath = shared
		default:
			fmt.Fprintf(stdout, "gates: %s absent — gates unset, not applied\n", specific)
			return 0
		}
	}
	g, err := eval.LoadGates(gatesPath)
	if err != nil {
		fmt.Fprintf(stderr, "tinybouncer eval: %v\n", err)
		return 1
	}
	if v := g.Violations(current); len(v) > 0 {
		for _, line := range v {
			fmt.Fprintln(stderr, "gates: "+line)
		}
		fmt.Fprintln(stdout, "gates: FAILED (details on stderr)")
		return gatesExit
	}
	fmt.Fprintln(stdout, "gates: PASS")
	return 0
}

// runSweep evaluates each --sweep variant as a normal run (report + history
// line each), then prints the operating-point summary table. Only the jev
// backend supports the TINY_BOUNCER_JEV_THRESHOLDS override; any other backend
// fails with a clear error.
func runSweep(b backend.Backend, info backend.Info, corpusFlag, reportsFlag, sweepSpec string, stdout, stderr io.Writer) int {
	if info.Name != "jev" {
		fmt.Fprintf(stderr, "tinybouncer eval: backend %q has no threshold override support; --sweep requires --backend jev\n", info.Name)
		return 1
	}
	var variants []string
	for _, v := range strings.Split(sweepSpec, ";") {
		if strings.TrimSpace(v) != "" {
			variants = append(variants, strings.TrimSpace(v))
		}
	}

	set, err := corpusLoader(corpusFlag)
	if err != nil {
		fmt.Fprintf(stderr, "tinybouncer eval: %v\n", err)
		return 1
	}

	original, hadOriginal := os.LookupEnv(thresholdsEnv)
	defer func() {
		if hadOriginal {
			os.Setenv(thresholdsEnv, original)
		} else {
			os.Unsetenv(thresholdsEnv)
		}
	}()

	type row struct {
		variant string
		history eval.HistoryLine
	}
	var rows []row
	for _, v := range variants {
		if err := os.Setenv(thresholdsEnv, v); err != nil {
			fmt.Fprintf(stderr, "tinybouncer eval: set %s: %v\n", thresholdsEnv, err)
			return 2
		}
		vb, err := factoryFor("jev")
		if err != nil {
			fmt.Fprintf(stderr, "tinybouncer eval: --sweep variant %q: %v\n", v, err)
			return 1
		}
		scores, usage, err := eval.RunBackend(context.Background(), vb, set)
		if err != nil {
			fmt.Fprintf(stderr, "tinybouncer eval: internal error: %v\n", err)
			return 2
		}
		rep := eval.BuildReport(time.Now(), vb.Info(), set, scores, nil, usage)
		if _, err := rep.Write(reportsFlag); err != nil {
			fmt.Fprintf(stderr, "tinybouncer eval: write report: %v\n", err)
			return 2
		}
		if err := eval.AppendHistory(reportsFlag, rep.History()); err != nil {
			fmt.Fprintf(stderr, "tinybouncer eval: history %q: %v\n", reportsFlag, err)
			return 2
		}
		rows = append(rows, row{variant: v, history: rep.History()})
	}

	fmt.Fprintln(stdout, "sweep (operating points, route threshold constants varied):")
	fmt.Fprintln(stdout, "  variant | sensitivity  f1      fnr     fpr     accuracy3  lat_p95_ms")
	for _, r := range rows {
		fmt.Fprintf(stdout, "  %-64s %.4f  %.4f  %.4f  %.4f  %.4f     %.1f\n",
			r.variant, r.history.Sensitivity, r.history.F1, r.history.FNR,
			r.history.FPR, r.history.Accuracy3, r.history.LatP95)
	}
	return 0
}

// corpusLoader locates and loads the eval corpus. An empty path resolves to
// data/evalset.json searched upwards from the working directory and then the
// binary's directory, so `bin/tinybouncer eval` also works from inside the tree.
func corpusLoader(corpusPath string) (*eval.Set, error) {
	if corpusPath == "" {
		corpusPath = findCorpus()
		if corpusPath == "" {
			return nil, fmt.Errorf(
				"corpus not found: no data/evalset.json above the working directory; pass --corpus <file>")
		}
	}
	return eval.LoadEvalSet(corpusPath)
}

// findCorpus walks upwards from the working directory, then the binary's
// directory, looking for data/evalset.json.
func findCorpus() string {
	dirs := []string{""}
	if cwd, err := os.Getwd(); err == nil {
		dirs = append(dirs, cwd)
	}
	if self, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(self))
	}
	for _, start := range dirs {
		dir := start
		for {
			cand := filepath.Join(dir, "data", "evalset.json")
			if _, err := os.Stat(cand); err == nil {
				return cand
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return ""
}
