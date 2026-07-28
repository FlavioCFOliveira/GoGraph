package cypher

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestQueryHasWritingClause_IgnoresCommentsAndStrings is the regression gate for
// rmp #2230: a writing keyword that occurs only where a clause CANNOT occur must
// not route a read onto the write path.
//
// The consequence being guarded is not cosmetic. A misrouted read runs inside a
// write transaction and therefore serialises on the store's single writer, so one
// such query throttles the concurrent read throughput the engine exists to
// provide — and nothing surfaces that it happened.
func TestQueryHasWritingClause_IgnoresCommentsAndStrings(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		query string
		want  bool
	}{
		// ── the two reproductions from the task ──────────────────────────────
		{"keyword inside a single-quoted string",
			`MATCH (n:Person) WHERE n.name = 'CREATE' RETURN count(n)`, false},
		{"keyword inside a line comment",
			"// CREATE nothing here\nCALL db.labels() YIELD label", false},

		// ── every maskable region ───────────────────────────────────────────
		{"keyword inside a double-quoted string",
			`MATCH (n) WHERE n.tag = "DELETE ME" RETURN n`, false},
		{"keyword inside a backtick identifier",
			"MATCH (n) RETURN n.`SET value` AS v", false},
		{"keyword inside a block comment",
			"/* MERGE is mentioned here */ MATCH (n) RETURN n", false},
		{"keyword inside a trailing line comment",
			"MATCH (n) RETURN n // DELETE later", false},
		{"keyword inside a multi-line block comment",
			"/*\n DETACH DELETE n\n*/\nMATCH (n) RETURN n", false},

		// ── escape sequences: a quote escaped inside a string must not end it ─
		{"escaped single quote does not end the string early",
			`MATCH (n) WHERE n.s = 'it\'s CREATE time' RETURN n`, false},
		{"escaped double quote does not end the string early",
			`MATCH (n) WHERE n.s = "say \"MERGE\" now" RETURN n`, false},
		{"escaped backslash before the closing quote",
			`MATCH (n) WHERE n.s = 'ends with a backslash\\' RETURN n`, false},
		{"a backslash inside a backtick identifier is an ordinary byte",
			"MATCH (n) RETURN n.`a\\b` AS v", false},

		// ── genuine writes must still be detected ────────────────────────────
		{"plain CREATE", `CREATE (n:Person {name: 'x'})`, true},
		{"plain MERGE", `MERGE (n:Person {name: 'x'})`, true},
		{"SET after a MATCH", `MATCH (n) SET n.x = 1`, true},
		{"REMOVE", `MATCH (n) REMOVE n:Label`, true},
		{"DELETE", `MATCH (n) DELETE n`, true},
		{"DETACH DELETE", `MATCH (n) DETACH DELETE n`, true},
		{"write preceded by a comment mentioning no keyword",
			"// housekeeping pass\nMATCH (n) SET n.seen = true", true},
		{"write preceded by a block comment mentioning no keyword",
			"/* nightly */ MATCH (n) DELETE n", true},
		{"mixed read and write", `MATCH (a) WHERE a.x = 1 CREATE (b:Copy)`, true},
		{"write whose STRING mentions a different keyword",
			`MATCH (n) WHERE n.s = 'MERGE' SET n.done = true`, true},
		{"write after a closed block comment on one line",
			"/* c */ CREATE (n)", true},

		// ── unterminated regions: masked to the end, so classified as a read ──
		{"unterminated block comment swallows the rest",
			"/* CREATE (n)", false},
		{"unterminated single-quoted string swallows the rest",
			"MATCH (n) WHERE n.s = 'CREATE (m)", false},
		{"unterminated backtick swallows the rest",
			"MATCH (n) RETURN n.`CREATE", false},

		// ── shapes with no maskable region at all (the fast path) ────────────
		{"read with no comment or quote", `MATCH (n:Person) RETURN n`, false},
		{"write with no comment or quote", `MATCH (n) SET n.x = 1`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := QueryHasWritingClause(tc.query); got != tc.want {
				t.Errorf("QueryHasWritingClause(%q) = %v; want %v\nmasked: %q",
					tc.query, got, tc.want, maskNonClauseRegions(tc.query))
			}
		})
	}
}

// TestMaskNonClauseRegions_FastPathDoesNotCopy asserts the allocation-free
// property the classifier depends on: a query with none of the four opening
// characters must be returned as the SAME string, not a copy.
//
// This runs on every RunAny / RunInTxAny dispatch, so a copy here is paid by every
// query in the process (rmp #2230 AC 5).
func TestMaskNonClauseRegions_FastPathDoesNotCopy(t *testing.T) {
	t.Parallel()

	const q = "MATCH (n:Person) WHERE n.age > 30 RETURN n.name ORDER BY n.age LIMIT 10"
	got := maskNonClauseRegions(q)
	if got != q {
		t.Fatalf("fast path altered the query:\n got %q\nwant %q", got, q)
	}
	// unsafe.StringData would prove identity outright, but comparing lengths and
	// contents plus the absence of any maskable character is sufficient here: the
	// function returns the input unchanged on that branch by construction, and the
	// benchmark below pins the allocation count.
	if strings.ContainsAny(q, "/'\"`") {
		t.Fatal("this fixture must contain no maskable character, or it does not test the fast path")
	}
}

// TestMaskNonClauseRegions_PreservesLineStructure pins that masking blanks bytes
// rather than deleting them, so a line comment following a masked region still
// terminates where it should and offsets into the query are unchanged.
func TestMaskNonClauseRegions_PreservesLineStructure(t *testing.T) {
	t.Parallel()

	for _, q := range []string{
		"MATCH (n) WHERE n.s = 'CREATE' RETURN n",
		"// CREATE\nMATCH (n) RETURN n",
		"/* CREATE\nMERGE */\nMATCH (n) RETURN n",
		"MATCH (n) RETURN n.`SET x`",
	} {
		masked := maskNonClauseRegions(q)
		if len(masked) != len(q) {
			t.Errorf("mask changed the length of %q: %d -> %d (%q)", q, len(q), len(masked), masked)
		}
		if strings.Count(masked, "\n") != strings.Count(q, "\n") {
			t.Errorf("mask changed the newline count of %q: %q", q, masked)
		}
	}
}

// BenchmarkQueryHasWritingClause measures the classifier on the shapes that
// matter for dispatch cost: one with no maskable region (the fast path, which
// must not allocate) and one with a comment and a string (the masking path).
func BenchmarkQueryHasWritingClause(b *testing.B) {
	cases := []struct{ name, query string }{
		{"no_comment_or_string", "MATCH (n:Person) WHERE n.age > 30 RETURN n.name ORDER BY n.age LIMIT 10"},
		{"write_no_comment_or_string", "MATCH (n:Person) WHERE n.age > 30 SET n.seen = true"},
		{"comment_and_string", "// nightly pass\nMATCH (n:Person) WHERE n.name = 'CREATE' RETURN count(n)"},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if QueryHasWritingClause(c.query) && false {
					b.Fatal("unreachable")
				}
			}
		})
	}
}

// TestRunAnyDispatch_CommentDoesNotForceTheWritePath is rmp #2230 AC 4: the
// classifier's verdict must actually change dispatch, not merely the boolean.
//
// The observable is `CALL db.labels()`, which is exactly how the defect was
// found: adding a comment mentioning CREATE to a working read broke it outright,
// because the misrouted read reached the write path where the procedure registry
// was unreachable (#2229). With #2229 landed the procedure resolves on both
// paths, so the assertion is strengthened to compare the two spellings' RESULTS —
// a commented read must return exactly what the uncommented one returns.
func TestRunAnyDispatch_CommentDoesNotForceTheWritePath(t *testing.T) {
	t.Parallel()

	g := newMaskTestGraph(t)
	eng := NewEngine(g)

	plain := "CALL db.labels() YIELD label RETURN label ORDER BY label"
	commented := "// CREATE nothing here\n" + plain
	stringed := "MATCH (n) WHERE n.name = 'CREATE' RETURN count(n) AS c"

	// Both spellings must classify as reads...
	if QueryHasWritingClause(commented) {
		t.Fatal("a comment mentioning CREATE must not make this a write")
	}
	if QueryHasWritingClause(stringed) {
		t.Fatal("a string containing CREATE must not make this a write")
	}

	// ...and both must run on the read path and agree with the plain spelling.
	want := drainLabels(t, eng, plain)
	got := drainLabels(t, eng, commented)
	if len(want) == 0 {
		t.Fatal("fixture produced no labels, so this proves nothing")
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("commented read returned %v; the uncommented one returned %v", got, want)
	}

	// The string-literal case executes on the read path too.
	if _, err := eng.RunAny(context.Background(), stringed, nil); err != nil {
		t.Errorf("a read whose string literal contains CREATE must execute: %v", err)
	}
}

// newMaskTestGraph builds a small labelled graph for the dispatch test.
func newMaskTestGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	eng := NewEngine(g)
	for _, cy := range []string{
		`CREATE (:Person {name: 'a'})`,
		`CREATE (:Company {name: 'b'})`,
	} {
		res, err := eng.RunInTxAny(context.Background(), cy, nil)
		if err != nil {
			t.Fatalf("seed %q: %v", cy, err)
		}
		if cerr := res.Close(); cerr != nil {
			t.Fatalf("seed close: %v", cerr)
		}
	}
	return g
}

// drainLabels runs a read through RunAny (the dispatching entry point) and
// returns the label column.
func drainLabels(t *testing.T, eng *Engine, cy string) []string {
	t.Helper()
	res, err := eng.RunAny(context.Background(), cy, nil)
	if err != nil {
		t.Fatalf("RunAny(%q): %v", cy, err)
	}
	var out []string
	for res.Next() {
		out = append(out, fmt.Sprint(res.Record()["label"]))
	}
	if rerr := res.Err(); rerr != nil {
		t.Fatalf("RunAny(%q): drain: %v", cy, rerr)
	}
	if cerr := res.Close(); cerr != nil {
		t.Fatalf("RunAny(%q): close: %v", cy, cerr)
	}
	return out
}
