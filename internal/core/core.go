// Package core defines the stable, backend-blind contracts shared by the
// tinybouncer pipeline: effects, verdicts, and the commands being judged.
package core

// Effect is the permission outcome of a verdict: "allow", "deny", or "ask".
type Effect string

// The three verdict effects. Any deny dominates; else any ask; else allow.
const (
	Allow Effect = "allow"
	Deny  Effect = "deny"
	Ask   Effect = "ask"
)

// Verdict is the generic judgment for one command. Confidence is backend-native
// (0..1) and informational; each backend enforces its own certainty discipline.
type Verdict struct {
	Effect     Effect
	Confidence float64
	Categories []string
	Reason     string // one sentence, may be empty
}

// Command is a single shell command as requested by the agent.
type Command struct {
	Raw string
}
