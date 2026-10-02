// Command wiseyolo screens shell commands requested by LLM agents before they
// run, mapping verdicts onto OpenCode permission effects (architecture §3).
//
// Subcommands:
//
//	check  — one classification run; input JSON on stdin, output contract on stdout
//	eval   — corpus evaluation over a backend (task T09; not implemented yet)
//	doctor — backend health reporting (task T07; not implemented yet)
//
// Commands travel on stdin, not argv, so arbitrary quoting and long batches
// are safe. Exit codes: 0 success (including degraded verdicts — the JSON is
// the contract), 1 usage/config error, 2 internal error.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"wiseyolo/internal/backend"
	_ "wiseyolo/internal/backend/mock" // register the offline backend
	"wiseyolo/internal/dispatch"
)

// defaultBackend is the backend id used when neither the flag nor the
// environment names one (architecture §6).
const defaultBackend = "jev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run routes a subcommand and returns the process exit code. The reader and
// writer parameters keep the flow testable in-process.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 1
	}
	switch args[0] {
	case "check":
		return runCheck(args[1:], stdin, stdout, stderr)
	case "eval", "doctor":
		fmt.Fprintf(stderr, "wiseyolo %s: not implemented yet\n\n", args[0])
		usage(stderr)
		return 1
	case "help", "--help", "-h":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "wiseyolo: unknown subcommand %q\n\n", args[0])
		usage(stderr)
		return 1
	}
}

// usage prints the subcommand summary.
func usage(w io.Writer) {
	fmt.Fprintf(w, `usage: wiseyolo <subcommand> [flags]

subcommands:
  check   classify commands read from stdin (JSON: {"commands": [...]})
  eval    run the labelled corpus against a backend (not implemented yet)
  doctor  verify backend credentials and reachability (not implemented yet)

check flags:
  --backend <name>   judgment backend (default: WISE_YOLO_BACKEND or %[1]s)
  --cache            force the response cache on (not effective until T04)
  --no-cache         force the response cache off (not effective until T04)
`, defaultBackend)
}

// inputError is a stable JSON error object for stdin/validation failures so
// callers diagnosing a rejected request get machine-readable output too.
type inputError struct {
	Error string `json:"error"`
}

// writeInputError emits the JSON error on stdout and a plain line on stderr,
// for a usage/config exit (plan T03: valid JSON error diagnosing).
func writeInputError(stdout, stderr io.Writer, msg string) {
	e := inputError{Error: msg}
	if b, err := json.MarshalIndent(e, "", "  "); err == nil {
		fmt.Fprintln(stdout, string(b))
	}
	fmt.Fprintf(stderr, "wiseyolo check: %s\n", msg)
}

// runCheck implements the check subcommand (architecture §3 and §5).
func runCheck(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	backendName := fs.String("backend", backend.Env("WISE_YOLO_BACKEND", defaultBackend),
		"judgment backend id (registry name)")
	cacheOn := fs.Bool("cache", false, "force the response cache on")
	cacheOff := fs.Bool("no-cache", false, "force the response cache off")
	// Both flags are parsed here and deliberately ineffective until the
	// response cache lands in task T04 (plan T03).
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *cacheOn && *cacheOff {
		fs.Usage()
		return 1
	}

	factory, ok := backend.Lookup(*backendName)
	if !ok {
		fmt.Fprintf(stderr, "wiseyolo check: unknown backend %q (available: %s)\n",
			*backendName, availableBackends())
		return 1
	}
	b, err := factory(backend.Config{})
	if err != nil {
		fmt.Fprintf(stderr, "wiseyolo check: backend %q: %v\n", *backendName, err)
		return 1
	}

	var input struct {
		Commands []string `json:"commands"`
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		writeInputError(stdout, stderr, fmt.Sprintf("could not read stdin: %v", err))
		return 1
	}
	if err := json.Unmarshal(data, &input); err != nil {
		writeInputError(stdout, stderr, fmt.Sprintf("malformed stdin: %v", err))
		return 1
	}
	if len(input.Commands) > dispatch.MaxCommands {
		writeInputError(stdout, stderr, fmt.Sprintf(
			"input rejected: %d commands exceeds the limit of %d per invocation",
			len(input.Commands), dispatch.MaxCommands))
		return 1
	}

	out := dispatch.New(b, nil).Run(context.Background(), input.Commands)
	enc, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "wiseyolo check: internal error encoding the contract: %v\n", err)
		return 2
	}
	fmt.Fprintln(stdout, string(enc))
	return 0
}

// availableBackends lists the registered backend ids for diagnostics.
func availableBackends() string {
	return strings.Join(backend.Names(), ", ")
}
