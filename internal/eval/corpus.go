// Package eval implements the corpus evaluation layer: the labelled synthetic
// corpus loader and validation (this file), metrics, runs, and reports
// (architecture §7).
package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Truth is the labelled ground-truth effect for a corpus record.
type Truth string

// The three ground-truth values, matching the verdict effects.
const (
	TruthAllow Truth = "allow"
	TruthAsk   Truth = "ask"
	TruthDeny  Truth = "deny"
)

// Truths is the exhaustive ground-truth vocabulary.
var Truths = []Truth{TruthAllow, TruthAsk, TruthDeny}

// Categories is the fixed record-category vocabulary (architecture §7, T08).
// Values are mirrored in data/evalset.json under _meta.categories; the test
// suite cross-checks the two.
var Categories = []string{
	"vcs_read",
	"fs_read",
	"build_test",
	"vcs_destructive",
	"fs_destructive",
	"system_security",
	"remote_exec",
	"exfiltration",
	"service_disruption",
	"package_install",
	"sudo",
	"disguised",
	"scary_but_safe",
	"borderline",
}

// DisguisedCategory marks records that are innocent-looking-but-not or
// scary-looking-but-safe; they are scored as a separate subset.
const DisguisedCategory = "disguised"

// Record is one labelled corpus entry. Comment is present only in few-shot
// records; scoring excludes those.
type Record struct {
	ID         string   `json:"id"`
	Command    string   `json:"command"`
	Truth      Truth    `json:"truth"`
	Categories []string `json:"categories"`
	Notes      string   `json:"notes"`
	Comment    string   `json:"comment,omitempty"`
}

// Meta carries corpus-level facts; only the category list is meaningful to
// the loader.
type Meta struct {
	Comment string `json:"comment,omitempty"`
}

// Set is a parsed JSON file of records, either the eval corpus or the
// few-shot subset. CategoryVocabulary is read for cross-checking.
type Set struct {
	Records            []Record
	CategoryVocabulary []string
}

// evalSetFile mirrors evalset.json on disk.
type evalSetFile struct {
	Meta *struct {
		Comment    string   `json:"comment"`
		Categories []string `json:"categories"`
	} `json:"_meta"`
	Records []Record `json:"records"`
}

// fewShotFile mirrors fewshot.json on disk.
type fewShotFile struct {
	Meta    *Meta    `json:"_meta"`
	Records []Record `json:"records"`
}

// LoadEvalSet reads and validates data/evalset.json.
func LoadEvalSet(path string) (*Set, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f evalSetFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("evalset schema: JSON parse: %w", err)
	}
	set := &Set{Records: f.Records}
	if f.Meta != nil {
		set.CategoryVocabulary = f.Meta.Categories
	}
	if err := strictSchema[evalSetFile](raw); err != nil {
		return nil, fmt.Errorf("evalset schema: %w", err)
	}
	if err := Validate(set); err != nil {
		return nil, err
	}
	return set, nil
}

// LoadFewshot reads and validates data/fewshot.json. validateCross is set by
// the caller only when records must not collide with the eval corpus.
func LoadFewshot(path string) (*Set, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f fewShotFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("fewshot schema: JSON parse: %w", err)
	}
	set := &Set{Records: f.Records, CategoryVocabulary: Categories}
	if err := strictSchema[fewShotFile](raw); err != nil {
		return nil, fmt.Errorf("fewshot schema: %w", err)
	}
	if err := Validate(set); err != nil {
		return nil, err
	}
	return set, nil
}

// strictSchema re-decodes raw with DisallowUnknownFields so that records
// carrying keys outside the record contract are rejected, not silently
// ignored.
func strictSchema[T any](raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	t := new(T)
	return dec.Decode(t)
}

// Validate checks a set against the architecture §7 record contract: valid
// truth enum values, known category values, non-empty categories and
// commands, and unique ids across the file. Where the file mirrors the
// category vocabulary, that mirror must equal the code's vocabulary. It
// returns one aggregated error listing every violation found.
func Validate(set *Set) error {
	if set == nil {
		return fmt.Errorf("corpus: nil set")
	}
	vocab := map[string]bool{}
	for _, c := range Categories {
		vocab[c] = true
	}
	// Mirror vocabulary from the file, when present, must equal the code's.
	var problems []string
	if len(set.CategoryVocabulary) > 0 {
		if len(set.CategoryVocabulary) != len(Categories) {
			problems = append(problems,
				fmt.Sprintf("vocabulary mismatch: file lists %d categories, code defines %d",
					len(set.CategoryVocabulary), len(Categories)))
		}
		fileVocab := map[string]bool{}
		for _, c := range set.CategoryVocabulary {
			fileVocab[c] = true
		}
		for _, c := range Categories {
			if !fileVocab[c] {
				problems = append(problems, "vocabulary mismatch: code category %q missing from file", c)
			}
		}
	}

	seen := map[string]bool{}
	for i, r := range set.Records {
		if r.ID == "" {
			problems = append(problems, fmt.Sprintf("record %d: empty id", i))
			continue
		}
		if seen[r.ID] {
			problems = append(problems, fmt.Sprintf("record %q: duplicate id", r.ID))
		}
		seen[r.ID] = true
		if r.Command == "" {
			problems = append(problems, fmt.Sprintf("record %q: empty command", r.ID))
		}
		switch r.Truth {
		case TruthAllow, TruthAsk, TruthDeny:
		default:
			problems = append(problems, fmt.Sprintf("record %q: truth %q not one of allow|ask|deny", r.ID, r.Truth))
		}
		if len(r.Categories) == 0 {
			problems = append(problems, fmt.Sprintf("record %q: no categories", r.ID))
		}
		have := map[string]bool{}
		for _, c := range r.Categories {
			if have[c] {
				problems = append(problems, fmt.Sprintf("record %q: duplicate category %q", r.ID, c))
			}
			have[c] = true
			if !vocab[c] {
				problems = append(problems, fmt.Sprintf("record %q: unknown category %q", r.ID, c))
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("corpus validation: %d problem(s):\n  %s", len(problems), joinLines(problems))
}

// FewshotCollision checks that no few-shot record id also appears in the
// eval corpus. Few-shot records are excluded from scoring; a shared id would
// be scored by mistake.
func FewshotCollision(corpus, fewshot *Set) error {
	in := map[string]bool{}
	for _, r := range corpus.Records {
		in[r.ID] = true
	}
	var bad []string
	for _, r := range fewshot.Records {
		if in[r.ID] {
			bad = append(bad, r.ID)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("fewshot ids also present in evalset: %v", bad)
	}
	return nil
}

// Totals counts records by truth value.
func Totals(set *Set) map[Truth]int {
	out := map[Truth]int{}
	for _, t := range Truths {
		out[t] = 0
	}
	for _, r := range set.Records {
		out[r.Truth]++
	}
	return out
}

// DisguisedCount returns the number of records in the separately scored
// disguised subset.
func DisguisedCount(set *Set) int {
	n := 0
	for _, r := range set.Records {
		for _, c := range r.Categories {
			if c == DisguisedCategory {
				n++
				break
			}
		}
	}
	return n
}

// ByCategory counts records per category (for the composition summary log).
func ByCategory(set *Set) map[string]int {
	out := map[string]int{}
	for _, r := range set.Records {
		for _, c := range r.Categories {
			out[c]++
		}
	}
	return out
}

func joinLines(lines []string) string {
	s := ""
	for i, l := range lines {
		if i > 0 {
			s += "\n  "
		}
		s += l
	}
	return s
}
