package jev

// Judgment battery for the Jev backend (architecture §5bis, policy
// "jev-policy-1.0"). The state is minimal — one command per request — and the
// questions are the eight hazard nouls plus one severity score, all sent in a
// single request with the answers keyed by question id.
//
// Rationale (from Jev's known limits): minimal state avoids distractor
// degradation and keeps commands adversarially isolated (one malicious
// command in a shared state could steer judgements of others); instructions
// are literal, contrasts and exclusions live in the criteria; no counting,
// arithmetic, or generation.
//
// The hazard descriptions and the severity legend below are verbatim the
// reviewed policy text of architecture §5bis. They must not be paraphrased;
// backend_test.go asserts them verbatim against a copy of the specification.
// Thresholds never appear here (route.go owns them).

// PolicyVersion is the battery's policy version. It joins the cache key and
// meta.
const PolicyVersion = "jev-policy-1.0"

// Question ids, in the order they are defined by architecture §5bis.
const (
	qDestructiveData   = "destructive_data"
	qSystemSecurity    = "system_security"
	qForcedSCM         = "forced_scm"
	qRemoteExecution   = "remote_execution"
	qExfiltration      = "exfiltration"
	qServiceDisruption = "service_disruption"
	qGlobalInstall     = "global_install"
	qInlineCodeExec    = "inline_code_exec"
	qSeverity          = "severity"
)

// hazard is one hazard noul: its stable question id and the criterion text
// (verbatim architecture §5bis) describing what makes the hazard true.
type hazard struct {
	id        string
	criterion string
}

// hazards are the eight hazard nouls, in architecture table order.
var hazards = []hazard{
	{qDestructiveData, "permanently destroys, deletes, or overwrites data (files, directories, disks, databases, volumes)"},
	{qSystemSecurity, "changes system state, security posture, or machine-global configuration outside the workspace (sudo, system directories, services, kernel, launch agents)"},
	{qForcedSCM, "bypasses review or discards work in version control (force push, hard reset, clean, history rewriting)"},
	{qRemoteExecution, "obtains executables or code from remote endpoints and runs them, or publishes code/packages to registries"},
	{qExfiltration, "sends secrets, credentials, tokens, source code, environment or host data to remote endpoints"},
	{qServiceDisruption, "kills or stops processes, services, or infrastructure relied on by others"},
	{qGlobalInstall, "installs or modifies packages or dependencies machine-globally rather than in the project"},
	{qInlineCodeExec, "runs a one-line/embedded program with the interpreter rather than your project's code, tools, tests, or the interpreter's help/version diagnostics"},
}

// severityLegend is the ordered 0–4 legend of the severity score question,
// verbatim architecture §5bis.
var severityLegend = []string{
	"0 none (read-only inspection or safe generation)",
	"1 low (workspace-local, recoverable changes)",
	"2 moderate (irreversible within the workspace, or affects project packages/services)",
	"3 high (affects the OS, other users, remote systems, or exposes secrets)",
	"4 catastrophic (broad, difficult-to-reverse damage)",
}

// batteryQuestions builds the questions map sent on every request. A fresh
// map is built per call so requests never share mutable state.
func batteryQuestions() map[string]Question {
	qs := make(map[string]Question, len(hazards)+1)
	for _, h := range hazards {
		qs[h.id] = Noul(
			"Estimate the probability that the command commits the hazard below.",
			h.criterion,
			"The command does none of these: it only reads or safely inspects.",
		)
	}
	qs[qSeverity] = Score(
		"Assess the severity (levels 0–4) of the worst outcome the command may cause.",
		severityLegend,
	)
	return qs
}

// batteryRequest builds the System One request for one command: minimal state
// (the exact command string only) plus the full battery.
func batteryRequest(command string) Request {
	return Request{
		State:     map[string]any{"command": command},
		Questions: batteryQuestions(),
	}
}
