// Package mock implements a deterministic, offline rules backend
// (architecture §5ter). It exists to exercise the whole pipeline — cache,
// thresholds, aggregation, CLI I/O — with no network, to give
// `eval --backend mock` a metrics-plumbing self-test and a floor to beat,
// and to document by example what a backend implements (architecture §5.2).
//
// The judgment payload is a fixed rule table versioned as policy "mock-0";
// the verdict mapping is the rule match itself, with certainty discipline
// inherent in the design: only unambiguous signatures deny, and unmatched
// commands fail conservative to ask (nothing within the mock ever
// auto-upgrades a deny or ask to allow). HealthCheck is trivially nil: a
// rules engine has no credentials and no reachability to verify.
//
// Rule evaluation order follows the plan's safe → dangerous → borderline →
// unmatched sequence, with one carve-out: a small set of severe signatures
// whose harm lies in the object rather than the tool (credential-file
// access, writes into /etc, piping anything into a shell) is evaluated
// before the safe prefixes, because otherwise `cat ~/.aws/credentials` or
// `echo X | sh` would be auto-allowed via the safe `cat` / `echo` prefixes
// and contradict the hard FNR = 0 goal (architecture §7).
//
// Matching is case-sensitive against the trimmed command, so flag patterns
// keep their literal shape (`chmod -R 777 /`); unusual casing falls through
// to the conservative unmatched → ask default.
package mock

import (
	"context"
	"strings"

	"tinybouncer/internal/backend"
	"tinybouncer/internal/core"
)

func init() {
	backend.Register("mock", factory)
}

func factory(cfg backend.Config) (backend.Backend, error) {
	return &mockBackend{}, nil
}

// info identifies the mock backend in meta, reports, and history.
var info = backend.Info{
	Name:              "mock",
	Model:             "mock-rules",
	PolicyVersion:     "mock-0",
	ThresholdsVersion: "mock-0",
}

type mockBackend struct{}

func (m *mockBackend) Name() string { return "mock" }

// Info returns the backend identity facts recorded alongside verdicts.
func (m *mockBackend) Info() backend.Info { return info }

// HealthCheck always succeeds: the mock backend has no credentials and no
// network dependency to verify.
func (m *mockBackend) HealthCheck(ctx context.Context) error { return nil }

// Classify judges each command against the rule table, returning one verdict
// per input, index-aligned and order-preserving, with one entry per input
// (the Backend contract, architecture §5). An empty batch yields an empty
// result. It never fails: unmatchable entries receive the ask verdict.
func (m *mockBackend) Classify(ctx context.Context, cmds []core.Command) ([]core.Verdict, error) {
	verdicts := make([]core.Verdict, 0, len(cmds))
	for _, cmd := range cmds {
		verdicts = append(verdicts, m.classify(cmd.Raw))
	}
	return verdicts, nil
}

func (m *mockBackend) classify(raw string) core.Verdict {
	cmd := strings.TrimSpace(raw)
	if rule := matchSevere(cmd); rule != nil {
		return rule.verdict()
	}
	if isSafePrefix(cmd) {
		return core.Verdict{
			Effect:     core.Allow,
			Confidence: 1.0,
			Categories: []string{"safe_read"},
			Reason:     "known-safe prefix matched",
		}
	}
	if rule := matchDeny(cmd); rule != nil {
		return rule.verdict()
	}
	if isBorderline(cmd) {
		return core.Verdict{
			Effect:     core.Ask,
			Confidence: 1.0,
			Categories: []string{"borderline"},
			Reason:     "borderline rule matched: requires human judgement",
		}
	}
	return unmatched()
}

type denyRule struct {
	// matched reports whether the trimmed command carries this rule's
	// dangerous signature.
	matched    func(cmd string) bool
	categories []string
	reason     string
}

// verdict builds the deny verdict for a matched rule (identical values for
// every rule; differing only in the reason line).
func (r *denyRule) verdict() core.Verdict {
	return core.Verdict{
		Effect:     core.Deny,
		Confidence: 1.0,
		Categories: r.categories,
		Reason:     "dangerous signature matched: " + r.reason,
	}
}

// severeRules are checked before the safe prefixes (see the package comment).
var severeRules = []denyRule{
	{contains(".aws/credentials"), []string{"exfiltration"}, "credential file read"},
	{etcWrite, []string{"system_security"}, "write into the system configuration directory"},
	{pipedIntoShell, []string{"remote_exec"}, "output piped into a shell execution"},
}

var denyRules = []denyRule{
	{prefixAny("rm -rf /", "rm -fr /", "rm -r /", "sudo rm"), []string{"fs_destructive"}, "recursive forcible deletion outside the workspace"},
	{prefixAny("rm -rf ~", "rm -rf $HOME"), []string{"fs_destructive"}, "recursive deletion of the home directory"},
	{contains("mkfs"), []string{"fs_destructive"}, "filesystem formatting"},
	{contains("of=/dev/"), []string{"fs_destructive"}, "raw write to a device node"},
	{contains(":(){"), []string{"service_disruption"}, "fork bomb"},
	{prefixAny("chmod -R 777 /", "chmod 777 /"), []string{"system_security"}, "recursive world-writable permissions from root"},
	{prefixAny("git push --force", "git push -f"), []string{"vcs_destructive"}, "version-control history destruction"},
	{prefixAny("git clean -f", "git clean -d"), []string{"vcs_destructive"}, "unversioned file removal"},
	{word("sudo"), []string{"sudo"}, "superuser privilege requested"},
	{prefixAny("python -c", "node -e"), []string{"remote_exec"}, "inline code execution"},
	{exact("npm publish"), []string{"package_install"}, "package publish to a registry"},
	{prefixAny("kill -9 1", "kill -9 -1"), []string{"service_disruption"}, "interruption of a process relied on by the system"},
	{prefixAny("terraform destroy"), []string{"service_disruption"}, "destroy of managed infrastructure"},
	{contains("drop database"), []string{"fs_destructive"}, "database dropped"},
}

// matchSevere returns the first severe deny rule matching the command.
func matchSevere(cmd string) *denyRule {
	for i := range severeRules {
		if severeRules[i].matched(cmd) {
			return &severeRules[i]
		}
	}
	return nil
}

// matchDeny returns the first deny rule matching the command.
func matchDeny(cmd string) *denyRule {
	for i := range denyRules {
		if denyRules[i].matched(cmd) {
			return &denyRules[i]
		}
	}
	return nil
}

var safePrefixes = []string{
	"git status", "git log", "git diff", "ls", "cat", "grep", "jq", "make",
	"go", "npm run", "npm test", "pytest", "cargo build", "docker ps",
	"df", "du", "ps", "uname", "echo",
}

// isSafePrefix reports whether the trimmed command starts with a known-safe
// token, on a word boundary (the token must end at a space or end of
// command, so "ps" does not match "psql").
func isSafePrefix(cmd string) bool {
	for _, p := range safePrefixes {
		if !strings.HasPrefix(cmd, p) {
			continue
		}
		if rest := cmd[len(p):]; rest == "" || rest[0] == ' ' {
			return true
		}
	}
	return false
}

var borderlinePrefixes = []string{
	"git reset --hard", "rm -rf ./node_modules", "apt install -y",
}

// isBorderline reports whether the command matches a genuinely borderline
// rule (evaluates to ask; see the package comment on evaluation order).
func isBorderline(cmd string) bool {
	for _, p := range borderlinePrefixes {
		if strings.HasPrefix(cmd, p) {
			return true
		}
	}
	return false
}

func prefixAny(prefixes ...string) func(string) bool {
	return func(cmd string) bool {
		for _, p := range prefixes {
			if strings.HasPrefix(cmd, p) {
				return true
			}
		}
		return false
	}
}

func contains(sub string) func(string) bool {
	return func(cmd string) bool { return strings.Contains(cmd, sub) }
}

// word reports whether the first word of the command equals w.
func word(w string) func(string) bool {
	return func(cmd string) bool {
		fields := strings.Fields(cmd)
		return len(fields) > 0 && fields[0] == w
	}
}

// exact reports whether the whole command equals s.
func exact(s string) func(string) bool {
	return func(cmd string) bool { return cmd == s }
}

// pipedIntoShell reports whether the command pipes output into a shell
// invocation (sh, bash, zsh), whatever the producer is.
func pipedIntoShell(cmd string) bool {
	shellWords := []string{"| sh", "|sh", "| bash", "|bash", "| zsh", "|zsh"}
	for _, s := range shellWords {
		if strings.Contains(cmd, s) {
			return true
		}
	}
	return false
}

// etcWrite reports whether the command is a redirection-style write into the
// system configuration directory.
func etcWrite(cmd string) bool {
	if !strings.Contains(cmd, "/etc/") && !strings.HasPrefix(cmd, "/etc") {
		return false
	}
	writeMarkers := []string{">", "tee", "cp ", "mv ", "touch ", "rm ", "dd ", "install "}
	for _, m := range writeMarkers {
		if strings.Contains(cmd, m) {
			return true
		}
	}
	return false
}

// unmatched builds the conservative default verdict when no rule matches.
func unmatched() core.Verdict {
	return core.Verdict{
		Effect:     core.Ask,
		Confidence: 0.25,
		Categories: []string{"unclassified"},
		Reason:     "no rule matched; defaulting to ask",
	}
}
