// This file holds the executable half of the docs/cypher.md gate: it runs
// every Cypher example the reference publishes, instead of merely checking that
// certain words appear in the text.
//
// WHY. The token gate in cypherdocgate_test.go asserts that five identifiers
// are mentioned. That is a word-presence check, so it cannot see a documented
// query that does not work. Round-4 audit finding C1 was exactly that: the
// worked example for the EXISTS subquery was rejected with a parse error and no
// gate noticed. Nothing in the project verified that any documented Cypher
// example ran, which made the defect class unbounded — C1 was simply the
// instance that happened to be found.
//
// CONTRACT. A fenced ```cypher block is executable documentation. Every
// statement in it is run, and must behave as the documentation implies:
// succeed, or fail in the way the documentation says it fails.
//
// A fence may carry directives after the language word:
//
//	```cypher gate:fixture=schema
//	```cypher gate:skip=illustrative grammar fragment, not a runnable query
//
// Within a block, statements are separated by a blank line, and a statement may
// declare that it is expected to be rejected:
//
//	SHOW CONSTRAINTS VERBOSE   // gate:error=SyntaxError
//
// Comments use `//`, which is Cypher's comment syntax and what the engine
// accepts. `--` is not a Cypher comment: a line starting with it is a parse
// error, so it must never appear in a published example.
package cypherdocgate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ---------------------------------------------------------------------------
// Extraction
// ---------------------------------------------------------------------------

// docExample is one statement extracted from a fenced Cypher block.
type docExample struct {
	doc       string // source document, repo-relative
	line      int    // 1-based line of the statement's first line
	block     int    // 1-based index of the fence within the document
	fixture   string // fixture name the block declared
	query     string // the statement text, comments included
	wantError string // non-empty when the statement must be rejected
}

// String identifies an example in test output.
func (e *docExample) String() string {
	return fmt.Sprintf("%s:%d (block %d)", e.doc, e.line, e.block)
}

var (
	fenceOpen  = regexp.MustCompile("^```cypher(\\s+.*)?$")
	fenceClose = regexp.MustCompile("^```\\s*$")
	// directives appear in a fence info string or a line comment.
	fixtureDirective = regexp.MustCompile(`gate:fixture=([A-Za-z0-9_-]+)`)
	skipDirective    = regexp.MustCompile(`gate:skip=(.*)$`)
	// The expected-error text runs to end of line, so it may contain spaces and
	// quotes: `gate:error=unsupported clause "VERBOSE"`. Capturing only a single
	// word here would silently weaken every expected-failure assertion to its
	// first word.
	errorDirective = regexp.MustCompile(`(?m)gate:error=(.+)$`)
)

// extractExamples walks the document and returns every runnable statement,
// together with the count of blocks skipped by an explicit directive.
func extractExamples(t *testing.T, doc, body string) (examples []docExample, skipped int) {
	t.Helper()

	lines := strings.Split(body, "\n")
	block := 0

	for i := 0; i < len(lines); i++ {
		m := fenceOpen.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		block++
		info := strings.TrimSpace(m[1])

		// Collect the fence body.
		start := i + 1
		end := start
		for end < len(lines) && !fenceClose.MatchString(lines[end]) {
			end++
		}
		if end >= len(lines) {
			t.Errorf("%s: fence opened at line %d is never closed", doc, i+1)
			return examples, skipped
		}
		fenceBody := lines[start:end]
		i = end // continue after the closing fence

		if sm := skipDirective.FindStringSubmatch(info); sm != nil {
			reason := strings.TrimSpace(sm[1])
			if reason == "" {
				t.Errorf("%s:%d: gate:skip must state a reason", doc, i+1)
			}
			skipped++
			continue
		}

		fixture := "social"
		if fm := fixtureDirective.FindStringSubmatch(info); fm != nil {
			fixture = fm[1]
		}

		examples = append(examples, splitStatements(doc, block, fixture, fenceBody, start+1)...)
	}
	return examples, skipped
}

// splitStatements divides a fence body into statements on blank lines and drops
// chunks that carry no Cypher (a comment-only chunk is a section heading, not a
// query). baseLine is the 1-based document line of fenceBody[0].
func splitStatements(doc string, block int, fixture string, fenceBody []string, baseLine int) []docExample {
	var (
		out     []docExample
		chunk   []string
		chunkAt int
	)

	flush := func() {
		if len(chunk) == 0 {
			return
		}
		text := strings.Join(chunk, "\n")
		chunk = nil
		if !hasCypher(text) {
			return // comment-only chunk: an annotation, not a statement
		}
		ex := docExample{doc: doc, line: chunkAt, block: block, fixture: fixture, query: text}
		if em := errorDirective.FindStringSubmatch(text); em != nil {
			ex.wantError = strings.TrimSpace(em[1])
		}
		out = append(out, ex)
	}

	for n, l := range fenceBody {
		if strings.TrimSpace(l) == "" {
			flush()
			continue
		}
		if len(chunk) == 0 {
			chunkAt = baseLine + n
		}
		chunk = append(chunk, l)
	}
	flush()
	return out
}

// hasCypher reports whether text contains anything other than blank lines and
// whole-line `//` comments.
func hasCypher(text string) bool {
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "//") {
			continue
		}
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// fixture seeds an engine that a block's statements run against. Each block
// gets its own engine, so a write example cannot perturb another block.
type fixture struct {
	setup  []string
	params map[string]any
}

// fixtures are the graphs a documented example may declare. Keep this set
// small: an example that needs something else should be rewritten to fit, so
// the reference stays coherent for a reader.
var fixtures = map[string]fixture{
	// empty — no data. For CREATE and MERGE examples that build their own.
	"empty": {
		params: commonParams(),
	},

	// social — the Alice/Bob/Carol world the reference narrates throughout.
	"social": {
		setup: []string{
			`CREATE (:Person {id: 1, name: 'Alice', age: 30, email: 'a@example.com', status: 'active'})`,
			`CREATE (:Person {id: 2, name: 'Bob', age: 25, email: 'b@example.com', status: 'active'})`,
			`CREATE (:Person {id: 3, name: 'Carol', age: 35, email: 'c@example.com', status: 'inactive'})`,
			`MATCH (a:Person {name: 'Alice'}), (b:Person {name: 'Bob'}) CREATE (a)-[:KNOWS {since: 2020}]->(b)`,
			`MATCH (b:Person {name: 'Bob'}), (c:Person {name: 'Carol'}) CREATE (b)-[:KNOWS {since: 2021}]->(c)`,
			`MATCH (a:Person {name: 'Alice'}), (c:Person {name: 'Carol'}) CREATE (a)-[:FOLLOWS]->(c)`,
			`CREATE (:City {name: 'Lisbon'})`,
		},
		params: commonParams(),
	},

	// schema — social plus the index and constraint objects the DDL section
	// shows being inspected and dropped.
	"schema": {
		setup: []string{
			`CREATE (:Person {id: 1, name: 'Alice', age: 30, email: 'a@example.com'})`,
			`CREATE (:Person {id: 2, name: 'Bob', age: 25, email: 'b@example.com'})`,
			`CREATE INDEX person_email FOR (n:Person) ON (n.email)`,
			`CREATE INDEX person_age FOR (n:Person) ON (n.age)`,
			`CREATE CONSTRAINT person_email_unique FOR (n:Person) REQUIRE n.email IS UNIQUE`,
			`CREATE CONSTRAINT person_name_notnull FOR (n:Person) REQUIRE n.name IS NOT NULL`,
		},
		params: commonParams(),
	},
}

// commonParams are the parameter bindings the reference's examples use. A
// documented example that references $x must find x here.
func commonParams() map[string]any {
	return map[string]any{
		"email": "a@example.com",
		"name":  "Alice",
		"age":   30,
		"id":    1,
		// items is a list of maps because the bulk-ingest example reads
		// item.sku and item.price from each element.
		"items": []any{
			map[string]any{"sku": "A-1", "price": 9.99},
			map[string]any{"sku": "B-2", "price": 19.50},
		},
		"names":  []any{"Alice", "Bob"},
		"param":  "Alice",
		"props":  map[string]any{"name": "Dave", "age": 40},
		"limit":  10,
		"since":  2020,
		"status": "active",
	}
}

// newEngine builds a fresh engine seeded with the named fixture.
func newEngine(t *testing.T, name string) (*cypher.Engine, map[string]any) {
	t.Helper()
	f, ok := fixtures[name]
	if !ok {
		t.Fatalf("unknown fixture %q; add it to fixtures or correct the gate:fixture directive", name)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	for _, s := range f.setup {
		res, err := eng.RunInTxAny(context.Background(), s, nil)
		if err != nil {
			t.Fatalf("fixture %q setup %q: %v", name, s, err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			t.Fatalf("fixture %q setup %q: %v", name, s, err)
		}
		_ = res.Close()
	}
	return eng, f.params
}

// ---------------------------------------------------------------------------
// The gate
// ---------------------------------------------------------------------------

// gatedDocs are the documents whose Cypher examples must execute.
var gatedDocs = []string{
	filepath.Join("docs", "cypher.md"),
}

// historicalDocs publish Cypher that is a record of a past run rather than a
// worked example for a reader: audit findings, benchmark write-ups, and design
// notes. Their queries are deliberately NOT executed — several describe
// behaviour as it was at the time, and re-running them would assert the present
// against a historical claim.
//
// Every document under docs/ that contains a ```cypher fence must appear in
// either gatedDocs or here; TestEveryDocWithCypherIsClassified enforces that,
// so a new document cannot quietly escape the gate.
var historicalDocs = map[string]bool{
	filepath.Join("docs", "audit-production-readiness-2026-07-02-round2.md"): true,
	// Quotes the two aggregation shapes whose CPU profile attributed 17.16% of all
	// samples to materialising a relationship they do not name, to identify the
	// shapes under discussion. They are verbatim from examples/26_social_scale_bench
	// and are exercised there; executing them here would assert nothing about the
	// record, whose figures are timings of that example at a specific commit.
	filepath.Join("docs", "certification-2026-08-09.md"):                true,
	filepath.Join("docs", "benchmarks", "bound-key-seek-2026-07-26.md"): true,
	// Quotes the CREATE INDEX that used to build an index holding no entries,
	// to show what the defect looked like. Running it would succeed and
	// demonstrate nothing, since the statement was always valid — it was the
	// resulting index that was empty.
	filepath.Join("docs", "benchmarks", "index-key-type-2026-07-27.md"): true,
	// Quotes the bulk-load statement whose plan was measured, to name the shape
	// under discussion. Running it would prove nothing about the record: the
	// figures are timings of that statement against a specific fixture at a
	// specific commit, which the harnesses in bench/r4audit and bench/comparison
	// reproduce, not this gate.
	filepath.Join("docs", "benchmarks", "write-path-hash-join-2026-07-27.md"): true,
	// Quotes the COUNT/EXISTS/size shapes whose plans were measured, to name the
	// eligibility boundary under discussion — including shapes that are
	// deliberately INELIGIBLE. Executing them would assert nothing about the
	// record; bench/r4audit/degree_test.go reproduces the figures.
	filepath.Join("docs", "benchmarks", "degree-rewrite-2026-07-27.md"): true,
	// Quotes the shortestPath shapes whose plans were measured, including ones
	// that deliberately fall back to the forward-only walk. Executing them would
	// assert nothing about the record; bench/r4audit/shortestpath_test.go
	// reproduces the figures.
	filepath.Join("docs", "benchmarks", "shortest-path-bidir-2026-07-27.md"): true,
	// Quotes the three reproductions of rmp #2316 — an edge CREATE and an edge
	// DELETE that a later clause of the same statement does not observe, and the
	// node CREATE that it does — to name the shapes the investigation is about.
	// Executing them here would assert nothing: they are valid Cypher that runs
	// cleanly, and what is under discussion is the COUNT each returns, which
	// this gate does not check. The behaviour itself is pinned in
	// cypher/writer_rows_test.go, deliberately as OBSERVED rather than correct,
	// so the remediation (rmp #2317) can change it.
	filepath.Join("docs", "audit-same-statement-edge-visibility-2026-08-02.md"): true,
}

// TestDocumentedCypherExamplesRun executes every Cypher example the gated
// documents publish. A documented query that fails to parse, or errors at
// evaluation, fails this test — which is what the token gate could not do.
func TestDocumentedCypherExamplesRun(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	for _, rel := range gatedDocs {
		raw, err := os.ReadFile(filepath.Join(root, rel)) //nolint:gosec // fixed in-repo doc path derived from go.mod location
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		examples, skipped := extractExamples(t, rel, string(raw))
		if len(examples) == 0 {
			t.Fatalf("%s: no runnable Cypher examples were extracted; the gate would pass vacuously", rel)
		}
		t.Logf("%s: %d runnable statements, %d blocks skipped by directive", rel, len(examples), skipped)

		// Group by block so each block gets one engine, and statements within
		// it run in document order — index and constraint examples depend on it.
		byBlock := map[int][]docExample{}
		var order []int
		for _, ex := range examples {
			if _, seen := byBlock[ex.block]; !seen {
				order = append(order, ex.block)
			}
			byBlock[ex.block] = append(byBlock[ex.block], ex)
		}

		for _, b := range order {
			group := byBlock[b]
			t.Run(fmt.Sprintf("%s/block%02d", strings.TrimSuffix(filepath.Base(rel), ".md"), b), func(t *testing.T) {
				t.Parallel()
				eng, params := newEngine(t, group[0].fixture)
				for i := range group {
					runExample(t, eng, params, &group[i])
				}
			})
		}
	}
}

// runExample executes one documented statement and reports any discrepancy
// between what happened and what the documentation implies.
func runExample(t *testing.T, eng *cypher.Engine, params map[string]any, ex *docExample) {
	t.Helper()
	if err := checkExample(eng, params, ex); err != nil {
		t.Error(err)
	}
}

// checkExample is the gate's decision, separated from test reporting so it can
// itself be tested (see TestGateRejectsABrokenExample). It returns nil when the
// statement behaved as documented, and a describing error otherwise.
func checkExample(eng *cypher.Engine, params map[string]any, ex *docExample) error {
	err := execute(eng, params, ex.query)

	switch {
	case ex.wantError != "" && err == nil:
		return fmt.Errorf("%s: documented as rejected (%s) but was accepted\n%s",
			ex, ex.wantError, indent(ex.query))
	case ex.wantError != "" && !strings.Contains(err.Error(), ex.wantError):
		return fmt.Errorf("%s: documented as failing with %s, but failed with:\n  %w\n%s",
			ex, ex.wantError, err, indent(ex.query))
	case ex.wantError == "" && err != nil:
		return fmt.Errorf("%s: documented example does not work:\n  %w\n%s",
			ex, err, indent(ex.query))
	}
	return nil
}

// execute runs query and drains it, returning the first error from either the
// call or the iteration.
//
// RunAny is deliberate: it is the dispatch helper the reference tells readers
// to use, so the gate exercises a documented example the way a reader would.
//
// It used to matter for correctness as well — a statement routed through the
// write path could not resolve a `CALL db.*` procedure, because the write-path
// plan builder did not thread the procedure registry. That was rmp #2229, found
// by this gate and fixed; both paths now resolve every registered procedure
// identically (cypher/proc_write_path_test.go). RunAny is kept purely because it
// is what the documentation tells readers to call.
func execute(eng *cypher.Engine, params map[string]any, query string) error {
	res, err := eng.RunAny(context.Background(), query, params)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		return err
	}
	return res.Close()
}

// indent renders a query for a failure message.
func indent(q string) string {
	var b strings.Builder
	for _, l := range strings.Split(q, "\n") {
		b.WriteString("    | ")
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}

// TestEveryDocWithCypherIsClassified walks docs/ and requires every document
// containing a ```cypher fence to be either gated or explicitly recorded as
// historical.
//
// This is what bounds the defect class the round-4 audit called out. Gating one
// file fixes one file; requiring every file to be classified means a new
// document that publishes Cypher must be a deliberate decision rather than an
// omission nobody notices.
func TestEveryDocWithCypherIsClassified(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	gated := map[string]bool{}
	for _, d := range gatedDocs {
		gated[d] = true
	}

	err := filepath.WalkDir(filepath.Join(root, "docs"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		raw, readErr := os.ReadFile(path) //nolint:gosec // walking a fixed in-repo docs tree
		if readErr != nil {
			return readErr
		}
		if !strings.Contains(string(raw), "```cypher") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if !gated[rel] && !historicalDocs[rel] {
			t.Errorf("%s publishes Cypher in a ```cypher fence but is neither gated nor "+
				"recorded as historical; add it to gatedDocs so its examples are executed, "+
				"or to historicalDocs with the reason it must not be", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk docs/: %v", err)
	}

	// A stale entry is also a defect: it would mask a document that later starts
	// publishing Cypher under a name already assumed handled.
	for rel := range historicalDocs {
		if _, statErr := os.Stat(filepath.Join(root, rel)); statErr != nil {
			t.Errorf("historicalDocs lists %s, which does not exist; remove the stale entry", rel)
		}
	}
}

// TestGateRejectsABrokenExample is the gate's own self-test: it proves the gate
// detects a documented query that does not work, which is the property the
// token gate lacked.
//
// It matters for a specific reason. This gate exists because of round-4 audit
// finding C1: docs/cypher.md published `WHERE EXISTS { MATCH (n)-[:KNOWS]->(m) }`
// as its worked example for the EXISTS subquery, and that query was rejected
// with a parse error. Task #2216 fixed the grammar, so C1's query now runs and
// can no longer make this gate fail. Asserting "the gate would have caught C1"
// therefore has to be done against a query the engine still rejects — otherwise
// the claim rests on nothing.
//
// Each case below is a document the gate must reject, and the reason it must.
func TestGateRejectsABrokenExample(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		doc  string
		why  string
	}{
		{
			name: "unparseable query",
			doc:  "```cypher\nMATCH (n RETURN n\n```\n",
			why:  "a syntax error in a published example",
		},
		{
			name: "unsupported feature presented as working",
			doc:  "```cypher\nMATCH (a:Person) CALL { WITH a RETURN a } RETURN a\n```\n",
			why:  "CALL { } subquery is not implemented, so this example cannot work",
		},
		{
			name: "evaluation error",
			doc:  "```cypher\nMATCH (n:Person) RETURN n.name.oops\n```\n",
			why:  "property access on a string fails at evaluation, not at parse time",
		},
		{
			name: "rejected example that actually succeeds",
			doc:  "```cypher\nMATCH (n:Person) RETURN n  // gate:error=SyntaxError\n```\n",
			why:  "documented as failing, but it works — the documentation is wrong either way",
		},
		{
			name: "rejected example that fails for the wrong reason",
			doc:  "```cypher\nMATCH (n RETURN n  // gate:error=ConstraintViolation\n```\n",
			why:  "documented as failing with one error, actually fails with another",
		},
		{
			// Guards the expected-error directive against silently capturing
			// only its first word: with a single-word capture this expectation
			// would degrade to "unsupported", which the real error does contain,
			// and the case would wrongly pass.
			name: "multi-word expected error that does not match",
			doc:  "```cypher\nSHOW CONSTRAINTS VERBOSE  // gate:error=unsupported clause \"BOGUS\"\n```\n",
			why:  "the whole expected-error text must be matched, not just its first word",
		},
		{
			name: "sql-style comment makes the example unparseable",
			doc:  "```cypher\n-- a comment\nMATCH (n:Person) RETURN n\n```\n",
			why:  "`--` is not a Cypher comment, so the example is not copy-pasteable",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			examples, _ := extractExamples(t, "synthetic.md", tc.doc)
			if len(examples) != 1 {
				t.Fatalf("expected 1 extracted statement, got %d", len(examples))
			}
			eng, params := newEngine(t, examples[0].fixture)
			if err := checkExample(eng, params, &examples[0]); err == nil {
				t.Errorf("the gate accepted a document it must reject (%s):\n%s", tc.why, tc.doc)
			}
		})
	}
}

// TestGateAcceptsAWorkingExample is the converse control: the gate must not
// report a discrepancy for a statement that behaves as documented. Without it,
// a gate that failed everything would pass the test above.
func TestGateAcceptsAWorkingExample(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, doc string }{
		{"working read", "```cypher\nMATCH (n:Person) RETURN n.name\n```\n"},
		{"C1's query, now fixed", "```cypher\nMATCH (n) WHERE EXISTS { MATCH (n)-[:KNOWS]->(m) } RETURN count(n)\n```\n"},
		{"correctly documented rejection", "```cypher\nSHOW CONSTRAINTS VERBOSE  // gate:error=unsupported clause\n```\n"},
		{"multi-word expected error that matches", "```cypher\nSHOW CONSTRAINTS VERBOSE  // gate:error=unsupported clause \"VERBOSE\"\n```\n"},
		{"comment-only chunk is not a statement", "```cypher\n// just a heading\nMATCH (n:Person) RETURN n\n```\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			examples, _ := extractExamples(t, "synthetic.md", tc.doc)
			if len(examples) != 1 {
				t.Fatalf("expected 1 extracted statement, got %d", len(examples))
			}
			eng, params := newEngine(t, examples[0].fixture)
			if err := checkExample(eng, params, &examples[0]); err != nil {
				t.Errorf("the gate rejected a valid documented example:\n%v", err)
			}
		})
	}
}

// TestNoSQLStyleCommentsInCypherExamples pins that no published example uses
// `--` as a comment. Cypher's comment is `//`; a line beginning with `--` is a
// parse error, so an example containing one is not copy-pasteable. The
// executable gate above would also catch it, but this reports the real cause
// instead of an opaque parse error.
func TestNoSQLStyleCommentsInCypherExamples(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	for _, rel := range gatedDocs {
		raw, err := os.ReadFile(filepath.Join(root, rel)) //nolint:gosec // fixed in-repo doc path derived from go.mod location
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		lines := strings.Split(string(raw), "\n")
		inFence := false
		for n, l := range lines {
			switch {
			case !inFence && fenceOpen.MatchString(l):
				inFence = true
			case inFence && fenceClose.MatchString(l):
				inFence = false
			case inFence && strings.HasPrefix(strings.TrimSpace(l), "--"):
				t.Errorf("%s:%d: Cypher example uses `--` as a comment; Cypher's comment is `//`\n    | %s",
					rel, n+1, l)
			}
		}
	}
}
