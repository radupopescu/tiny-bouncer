package eval

import (
	"os"
	"path/filepath"
	"testing"
)

// repoRoot walks up from the package directory to find go.mod, so the tests
// locate data/ both under `go test ./internal/eval/...` and `make test`.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above package directory")
		}
		dir = parent
	}
}

func loadCorpus(t *testing.T) (*Set, *Set) {
	t.Helper()
	root := repoRoot(t)
	corpus, err := LoadEvalSet(filepath.Join(root, "data", "evalset.json"))
	if err != nil {
		t.Fatalf("LoadEvalSet: %v", err)
	}
	fewshot, err := LoadFewshot(filepath.Join(root, "data", "fewshot.json"))
	if err != nil {
		t.Fatalf("LoadFewshot: %v", err)
	}
	return corpus, fewshot
}

func TestCorpusInvariants(t *testing.T) {
	corpus, fewshot := loadCorpus(t)

	if err := Validate(corpus); err != nil {
		t.Fatalf("corpus validation: %v", err)
	}
	if err := Validate(fewshot); err != nil {
		t.Fatalf("fewshot validation: %v", err)
	}
	if err := FewshotCollision(corpus, fewshot); err != nil {
		t.Fatalf("fewshot collision: %v", err)
	}

	// File mirror of the vocabulary matches the code vocabulary (recorded
	// via CategoryVocabulary during load).
	if len(corpus.CategoryVocabulary) != len(Categories) {
		t.Fatalf("_meta.categories length = %d, code vocabulary = %d",
			len(corpus.CategoryVocabulary), len(Categories))
	}
	fileVocab := map[string]bool{}
	for _, c := range corpus.CategoryVocabulary {
		fileVocab[c] = true
	}
	for _, c := range Categories {
		if !fileVocab[c] {
			t.Errorf("code category %q missing from _meta.categories", c)
		}
	}

	// Composition floors (plan T08).
	totals := Totals(corpus)
	if totals[TruthAllow] < 60 {
		t.Errorf("allow-truth records = %d, want >= 60", totals[TruthAllow])
	}
	if totals[TruthDeny] < 60 {
		t.Errorf("deny-truth records = %d, want >= 60", totals[TruthDeny])
	}
	if totals[TruthAsk] < 15 {
		t.Errorf("ask-truth records = %d, want >= 15", totals[TruthAsk])
	}
	if got := DisguisedCount(corpus); got < 30 {
		t.Errorf("disguised records = %d, want >= 30", got)
	}
	if total := len(corpus.Records); total < 195 {
		t.Errorf("corpus total = %d, want >= 195", total)
	}

	// Every deny-truth record carries at least one real hazard category
	// beyond the meta-labels.
	for _, r := range corpus.Records {
		if r.Truth != TruthDeny {
			continue
		}
		real := 0
		for _, c := range r.Categories {
			if c != DisguisedCategory {
				real++
			}
		}
		if real == 0 {
			t.Errorf("deny record %q has no real class category", r.ID)
		}
	}

	// Few-shot records are marked with a comment, per plan.
	for _, r := range fewshot.Records {
		if r.Comment == "" {
			t.Errorf("fewshot record %q has no comment field", r.ID)
		}
	}
	if len(fewshot.Records) < 12 {
		t.Errorf("fewshot records = %d, want >= ~16 authored", len(fewshot.Records))
	}

	logComposition(t, corpus, fewshot)
}

// logComposition prints the truth/category totals so a human can eyeball the
// corpus balance in the test output.
func logComposition(t *testing.T, corpus, fewshot *Set) {
	t.Helper()
	totals := Totals(corpus)
	t.Logf("corpus totals: allow=%d ask=%d deny=%d total=%d disguised=%d fewshot=%d",
		totals[TruthAllow], totals[TruthAsk], totals[TruthDeny],
		len(corpus.Records), DisguisedCount(corpus), len(fewshot.Records))
	for _, c := range ByCategory(corpus) {
		_ = c
	}
	ordered := Categories
	for _, c := range ordered {
		t.Logf("category %-18s %d", c, ByCategory(corpus)[c])
	}
}

func loadOK(t *testing.T, dir, name, content string) *Set {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if name == "fewshot.json" {
		s, err := LoadFewshot(path)
		if err != nil {
			t.Fatalf("LoadFewshot(%s): %v", name, err)
		}
		return s
	}
	s, err := LoadEvalSet(path)
	if err != nil {
		t.Fatalf("LoadEvalSet(%s): %v", name, err)
	}
	return s
}

func TestLoadRejects(t *testing.T) {
	dir := t.TempDir()

	cases := map[string]string{
		"bad truth":  `{"records":[{"id":"x1","command":"ls","truth":"maybe","categories":["fs_read"],"notes":"n"}]}`,
		"bad vocab":  `{"records":[{"id":"x2","command":"ls","truth":"allow","categories":["not_a_class"],"notes":"n"}]}`,
		"no classes": `{"records":[{"id":"x3","command":"ls","truth":"allow","categories":[],"notes":"n"}]}`,
		"empty cmd":  `{"records":[{"id":"x4","command":"","truth":"allow","categories":["fs_read"],"notes":"n"}]}`,
		"dup ids":    `{"records":[{"id":"x5","command":"ls","truth":"allow","categories":["fs_read"],"notes":"n"},{"id":"x5","command":"pwd","truth":"allow","categories":["fs_read"],"notes":"n"}]}`,
		"empty id":   `{"records":[{"id":"","command":"ls","truth":"allow","categories":["fs_read"],"notes":"n"}]}`,
		"bad json":   `{"records":[{"id":"x6",`,
		"vocabulary mismatch": `{"_meta":{"comment":"c","categories":["only_one"]},"records":[` +
			`{"id":"x7","command":"ls","truth":"allow","categories":["fs_read"],"notes":"n"}]}`,
	}
	for name, body := range cases {
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadEvalSet(path); err == nil {
			t.Errorf("%s: LoadEvalSet succeeded, want error", name)
		}
	}

	// Unknown fields in a record are counted as a schema error.
	strict := `{"records":[{"id":"x9","command":"ls","truth":"allow","categories":["fs_read"],"notes":"n","extra":true}]}`
	path := filepath.Join(dir, "strict.json")
	if err := os.WriteFile(path, []byte(strict), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEvalSet(path); err == nil {
		t.Errorf("unknown field: LoadEvalSet succeeded, want error")
	}
}

func TestValidateClean(t *testing.T) {
	dir := t.TempDir()
	body := `{"records":[{"id":"y1","command":"ls","truth":"allow","categories":["fs_read"],"notes":"n"}]}`
	s := loadOK(t, dir, "ok.json", body)
	if err := Validate(s); err != nil {
		t.Fatalf("Validate on clean set: %v", err)
	}
}

func TestFewshotCollisionHelper(t *testing.T) {
	corpus := &Set{Records: []Record{
		{ID: "e001", Command: "git status", Truth: TruthAllow, Categories: []string{"vcs_read"}},
	}}
	fewshot := &Set{Records: []Record{
		{ID: "f001", Command: "cat README.md", Truth: TruthAllow, Categories: []string{"fs_read"}, Comment: "seed"},
		{ID: "e001", Command: "cat README.md", Truth: TruthAllow, Categories: []string{"fs_read"}, Comment: "seed"},
	}}
	if err := FewshotCollision(corpus, fewshot); err == nil {
		t.Fatal("shared id not detected")
	}
	fewshot.Records = fewshot.Records[1:]
	if err := FewshotCollision(corpus, fewshot); err == nil {
		t.Fatal("shared id not detected after trim")
	}
	fewshot.Records = fewshot.Records[:1]
	fewshot.Records[0].ID = "f001"
	if err := FewshotCollision(corpus, fewshot); err != nil {
		t.Fatalf("clean sets reported collision: %v", err)
	}
}
