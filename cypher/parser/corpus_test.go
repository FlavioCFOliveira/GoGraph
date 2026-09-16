package parser

// corpus_test.go — the statement corpus the parsing laboratory measures.
//
// The corpus is testdata/examples-corpus.json: every Cypher statement the
// project's own examples issue, harvested from their Go source. It is the
// laboratory's fixed input, so a measurement taken today is comparable with one
// taken after a change: the corpus is versioned, the statements are not
// invented, and no example was modified to produce them.
//
// The corpus file's own "how" field records how it was harvested, what was
// materialised and why, and what it deliberately excludes. scripts/parse-lab.sh
// is the one command that runs the whole laboratory over it.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// corpusPath is the corpus file, relative to this package's directory.
const corpusPath = "testdata/examples-corpus.json"

// CorpusStatement is one harvested statement.
type corpusStatement struct {
	ID           string   `json:"id"`
	Example      string   `json:"example"`
	Source       string   `json:"source"`
	Shape        []string `json:"shape"`
	Bytes        int      `json:"bytes"`
	Materialised string   `json:"materialised,omitempty"`
	Text         string   `json:"text"`
}

type corpusFile struct {
	Schema            int               `json:"schema"`
	HarvestedAtCommit string            `json:"harvested_at_commit"`
	ExamplesScanned   []string          `json:"examples_scanned"`
	How               string            `json:"how"`
	Statements        []corpusStatement `json:"statements"`
}

// readCorpus decodes the corpus file. It fails the caller rather than
// returning an error: a laboratory with no input is not a laboratory.
func readCorpus(tb testing.TB) corpusFile {
	tb.Helper()
	raw, err := os.ReadFile(corpusPath)
	if err != nil {
		tb.Fatalf("read corpus %s: %v", corpusPath, err)
	}
	var f corpusFile
	if err := json.Unmarshal(raw, &f); err != nil {
		tb.Fatalf("decode corpus %s: %v", corpusPath, err)
	}
	if len(f.Statements) == 0 {
		tb.Fatalf("corpus %s is empty", corpusPath)
	}
	return f
}

// loadCorpus returns the corpus statements.
func loadCorpus(tb testing.TB) []corpusStatement {
	tb.Helper()
	return readCorpus(tb).Statements
}

// benchName turns a corpus id into a single Go sub-benchmark name segment, so
// the id's own separator does not become a benchmark-path separator.
func benchName(id string) string { return strings.ReplaceAll(id, "/", "-") }

// TestCorpusParses is the corpus's own gate: every statement in it must be one
// this package accepts, otherwise the laboratory would be measuring an error
// path and calling it a parse.
func TestCorpusParses(t *testing.T) {
	for _, s := range loadCorpus(t) {
		t.Run(benchName(s.ID), func(t *testing.T) {
			if _, err := Parse(s.Text); err != nil {
				t.Fatalf("%s (%s) does not parse: %v\nstatement: %s", s.ID, s.Source, err, s.Text)
			}
		})
	}
}

// TestCorpusTraceable asserts the property the campaign relies on: every
// statement names the example and the source position it came from, and the
// example is one of those the corpus declares it scanned.
func TestCorpusTraceable(t *testing.T) {
	f := readCorpus(t)
	scanned := make(map[string]bool, len(f.ExamplesScanned))
	for _, e := range f.ExamplesScanned {
		scanned[e] = true
	}
	seen := make(map[string]bool, len(f.Statements))
	for _, s := range f.Statements {
		switch {
		case s.ID == "":
			t.Errorf("statement from %s has no id", s.Source)
		case seen[s.ID]:
			t.Errorf("duplicate corpus id %s", s.ID)
		case !scanned[s.Example]:
			t.Errorf("%s names example %q, which the corpus does not declare as scanned", s.ID, s.Example)
		case !strings.HasPrefix(s.Source, "examples/"+s.Example+"/"):
			t.Errorf("%s: source %q does not lie inside its example", s.ID, s.Source)
		case s.Bytes != len(s.Text):
			t.Errorf("%s: bytes=%d but the text is %d bytes", s.ID, s.Bytes, len(s.Text))
		}
		seen[s.ID] = true
	}
}
