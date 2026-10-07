// Command failcheck is a test-only harness: it runs the identical dispatch
// pipeline with a backend whose Classify always fails, so subprocess contract
// tests can assert the unjudged→ask filling (architecture §5 step 4) against
// the same output-contract builder as check. It lives in testdata so
// production builds (./...) skip it entirely.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"tinybouncer/internal/backend"
	"tinybouncer/internal/core"
	"tinybouncer/internal/dispatch"
)

type failingBackend struct{}

func (failingBackend) Name() string { return "failing" }
func (failingBackend) Info() backend.Info {
	return backend.Info{
		Name:              "failing",
		Model:             "fail-model",
		PolicyVersion:     "fail-0",
		ThresholdsVersion: "fail-0",
	}
}
func (failingBackend) HealthCheck(context.Context) error { return nil }
func (failingBackend) Classify(context.Context, []core.Command) ([]core.Verdict, error) {
	return nil, errors.New("connection refused (test backend)")
}

func main() {
	var input struct {
		Commands []string `json:"commands"`
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failcheck: could not read stdin: %v\n", err)
		os.Exit(1)
	}
	if err := json.Unmarshal(data, &input); err != nil {
		fmt.Fprintf(os.Stderr, "failcheck: malformed stdin: %v\n", err)
		os.Exit(1)
	}
	out := dispatch.New(failingBackend{}, nil).Run(context.Background(), input.Commands)
	enc, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failcheck: %v\n", err)
		os.Exit(2)
	}
	fmt.Fprintln(os.Stdout, string(enc))
}
