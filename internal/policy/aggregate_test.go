package policy

import (
	"fmt"
	"testing"

	"wiseyolo/internal/core"
)

func TestAggregateEffect(t *testing.T) {
	cases := []struct {
		name string
		in   []core.Effect
		want core.Effect
	}{
		{"empty", nil, core.Allow},
		{"empty slice", []core.Effect{}, core.Allow},
		{"all allow", []core.Effect{core.Allow, core.Allow}, core.Allow},
		{"any ask", []core.Effect{core.Allow, core.Ask, core.Allow}, core.Ask},
		{"any deny wins", []core.Effect{core.Ask, core.Deny, core.Allow}, core.Deny},
		{"deny first", []core.Effect{core.Deny, core.Ask}, core.Deny},
		{"all deny", []core.Effect{core.Deny, core.Deny}, core.Deny},
		{"all ask", []core.Effect{core.Ask, core.Ask}, core.Ask},
		{"unknown effect tolerated", []core.Effect{"weird", core.Allow}, core.Allow},
		{"unknown with ask", []core.Effect{"weird", core.Ask}, core.Ask},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AggregateEffect(tc.in); got != tc.want {
				t.Fatalf("AggregateEffect(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAggregate(t *testing.T) {
	cases := []struct {
		name       string
		in         []core.Verdict
		wantEffect core.Effect
		wantReason string
		wantCats   []string
	}{
		{
			name:       "empty",
			in:         nil,
			wantEffect: core.Allow, wantReason: "", wantCats: []string{},
		},
		{
			name:       "single allow",
			in:         []core.Verdict{{Effect: core.Allow, Reason: "fine", Categories: []string{"vcs_read"}}},
			wantEffect: core.Allow, wantCats: []string{},
		},
		{
			name: "deny dominates with reason",
			in: []core.Verdict{
				{Effect: core.Allow},
				{Effect: core.Deny, Reason: "rm -rf / destroys data", Categories: []string{"fs_destructive"}},
			},
			wantEffect: core.Deny, wantReason: "rm -rf / destroys data", wantCats: []string{"fs_destructive"},
		},
		{
			name: "ask when no deny",
			in: []core.Verdict{
				{Effect: core.Allow},
				{Effect: core.Ask, Reason: "borderline git reset", Categories: []string{"borderline"}},
			},
			wantEffect: core.Ask, wantReason: "borderline git reset", wantCats: []string{"borderline"},
		},
		{
			name: "categories collected from same-effect verdicts",
			in: []core.Verdict{
				{Effect: core.Deny, Categories: []string{"exfiltration"}, Reason: "r1"},
				{Effect: core.Deny, Categories: []string{"exfiltration", "remote_exec"}, Reason: "r2"},
				{Effect: core.Allow, Categories: []string{"ignored"}},
			},
			wantEffect: core.Deny, wantReason: "r1", wantCats: []string{"exfiltration", "remote_exec"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Aggregate(tc.in)
			if got.Effect != tc.wantEffect {
				t.Fatalf("effect = %q, want %q", got.Effect, tc.wantEffect)
			}
			if got.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if fmt.Sprint(got.Categories) != fmt.Sprint(tc.wantCats) {
				t.Fatalf("categories = %v, want %v", got.Categories, tc.wantCats)
			}
		})
	}
}
