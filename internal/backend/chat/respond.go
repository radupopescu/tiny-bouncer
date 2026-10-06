package chat

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// respondTransport drives Apple Foundation Models through the official `fm`
// CLI: one `fm respond --no-stream -g --schema <file> -i <instructions>`
// subprocess per command, with the command on stdin. This is the reliable AFM
// path measured in plan T14 (the `fm serve` transport stalled under strict
// schema-constrained decoding).
type respondTransport struct {
	executable   string
	model        string
	instructions string
	schemaPath   string
}

func (t *respondTransport) classify(ctx context.Context, command string) (result, error) {
	args := []string{"respond", "--no-stream", "-g", "--schema", t.schemaPath, "-i", t.instructions}
	if t.model != "" {
		args = append(args, "-m", t.model)
	}
	cmd := exec.CommandContext(ctx, t.executable, args...)
	cmd.Stdin = strings.NewReader(command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return result{}, fmt.Errorf("fm respond failed: %v: %s", err, snippet(stderr.Bytes()))
	}
	out := bytes.TrimSpace(stdout.Bytes())
	if len(out) == 0 {
		return result{}, fmt.Errorf("fm respond produced no output")
	}
	return result{raw: out, model: t.model}, nil
}

func (t *respondTransport) health(ctx context.Context) error {
	if _, err := exec.LookPath(t.executable); err != nil {
		return fmt.Errorf("executable %q not found: %v", t.executable, err)
	}
	args := []string{"available"}
	if t.model != "" {
		args = append(args, "--model", t.model)
	}
	cmd := exec.CommandContext(ctx, t.executable, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("model unavailable: %s", unavailableReason(out.String(), err))
	}
	return nil
}

// unavailableReason extracts the human reason from `fm available` output
// ("System model unavailable: <reason>"), falling back to the command error.
func unavailableReason(out string, err error) string {
	const marker = "System model unavailable:"
	if i := strings.Index(out, marker); i >= 0 {
		if r := strings.TrimSpace(out[i+len(marker):]); r != "" {
			return r
		}
	}
	return err.Error()
}
