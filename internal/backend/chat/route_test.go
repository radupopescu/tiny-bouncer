package chat

import (
	"reflect"
	"strings"
	"testing"

	"tinybouncer/internal/core"
)

func TestMapAnswerTable(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		thr       *Thresholds
		wantErr   bool
		want      core.Effect
		wantConf  float64
		wantCats  []string
		wantInRea string
	}{
		{name: "allow stands", raw: `{"effect":"allow","confidence":0.9,"categories":["vcs_read"],"reason":"read only"}`,
			want: core.Allow, wantConf: 0.9, wantCats: []string{"vcs_read"}},
		{name: "allow below floor degrades to ask", raw: `{"effect":"allow","confidence":0.2,"categories":[],"reason":"maybe"}`,
			want: core.Ask, wantConf: 0.2, wantInRea: "allow floor"},
		{name: "deny stands", raw: `{"effect":"deny","confidence":0.9,"categories":["fs_destructive"],"reason":"destroys"}`,
			want: core.Deny, wantConf: 0.9, wantCats: []string{"fs_destructive"}},
		{name: "deny below floor degrades to ask", raw: `{"effect":"deny","confidence":0.3,"categories":["fs_destructive"],"reason":"unsure"}`,
			want: core.Ask, wantConf: 0.3, wantInRea: "deny floor"},
		{name: "ask stands", raw: `{"effect":"ask","confidence":0.1,"categories":["borderline"],"reason":"borderline"}`,
			want: core.Ask, wantConf: 0.1, wantCats: []string{"borderline"}},
		{name: "invalid effect is an error", raw: `{"effect":"maybe","confidence":1,"categories":[],"reason":"x"}`,
			wantErr: true},
		{name: "malformed json is an error", raw: `{not json`,
			wantErr: true},
		{name: "effect is trimmed and lower-cased", raw: `{"effect":" ALLOW ","confidence":1,"categories":[],"reason":"x"}`,
			want: core.Allow, wantConf: 1},
		{name: "unknown categories dropped, duplicates removed", raw: `{"effect":"deny","confidence":1,"categories":["sudo","bogus","sudo","fs_read",""],"reason":"x"}`,
			want: core.Deny, wantConf: 1, wantCats: []string{"sudo", "fs_read"}},
		{name: "confidence above one clamps", raw: `{"effect":"allow","confidence":1.7,"categories":[],"reason":"x"}`,
			want: core.Allow, wantConf: 1},
		{name: "confidence below zero clamps", raw: `{"effect":"deny","confidence":-0.5,"categories":[],"reason":"x"}`,
			thr:  &Thresholds{AllowFloor: 0, DenyFloor: 0},
			want: core.Deny, wantConf: 0},
		{name: "custom floors apply", raw: `{"effect":"allow","confidence":0.6,"categories":[],"reason":"x"}`,
			thr:  &Thresholds{AllowFloor: 0.8, DenyFloor: 0.2},
			want: core.Ask, wantConf: 0.6, wantInRea: "allow floor"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			thr := DefaultThresholds
			if tc.thr != nil {
				thr = *tc.thr
			}
			got, err := MapAnswer([]byte(tc.raw), thr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("MapAnswer(%s) = %+v, want error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("MapAnswer(%s) error: %v", tc.raw, err)
			}
			if got.Effect != tc.want {
				t.Errorf("effect = %q, want %q", got.Effect, tc.want)
			}
			if got.Confidence != tc.wantConf {
				t.Errorf("confidence = %v, want %v", got.Confidence, tc.wantConf)
			}
			if !reflect.DeepEqual(got.Categories, tc.wantCats) && !(len(got.Categories) == 0 && len(tc.wantCats) == 0) {
				t.Errorf("categories = %v, want %v", got.Categories, tc.wantCats)
			}
			if tc.wantInRea != "" && !strings.Contains(got.Reason, tc.wantInRea) {
				t.Errorf("reason = %q, want it to contain %q", got.Reason, tc.wantInRea)
			}
		})
	}
}

func TestCategoriesVocabularyNonEmpty(t *testing.T) {
	if len(Categories) < 14 {
		t.Fatalf("category vocabulary = %d, want at least 14", len(Categories))
	}
	set := categorySet()
	if _, ok := set["exfiltration"]; !ok {
		t.Errorf("vocabulary missing exfiltration: %v", Categories)
	}
}
