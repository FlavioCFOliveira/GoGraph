package cypher

import (
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
)

// This file is the regression gate for rmp #2242: the pattern form of a COUNT
// subquery silently dropped its inline WHERE, so `COUNT { (a)-[:K]->(b) WHERE p }`
// returned the UNFILTERED count.
//
// Root cause: ast.CountSubquery carried no Where field at all, where its
// ExistsSubquery sibling did, so VisitSubqueryCount parsed the clause (via
// visitPatternWhere) and then discarded it while building the node.
//
// The oracle throughout is the FULL subquery form, `COUNT { MATCH … WHERE … RETURN b }`,
// which routes through the ordinary MATCH pipeline and honoured the predicate all
// along, plus a hand-computed absolute value — because two forms agreeing proves
// nothing when the bug is in the form you trusted.
//
// # Why the oracle runs on its own engine (rmp #2647)
//
// The full form was trusted because no recogniser accepted that SPELLING, which
// makes the oracle a hostage to a limitation rather than to a decision. rmp #2648
// removes that limitation — it makes the block form rewritable — and the first
// case below (`COUNT { (a)-[:K]->(b) }`, no predicate on either side) is
// degree-answerable in both spellings. Both arms would then have taken the degree
// rewrite, and this file would have compared the rewrite with itself while
// staying green.
//
// The oracle arm is therefore an engine built with
// [EngineOptions.DisableAdjacencyCountRewrites], which forbids both
// adjacency-answered rewrites outright, and [oracleRun] asserts on the runtime
// counters that neither fired. It runs BOTH spellings: the block form, and the
// pattern form itself. Two independent unrewritten readings of the same question
// is what #2242's defect actually needed — the bug was that the PATTERN form
// dropped its predicate, so a reading of the pattern form that cannot take the
// rewrite is the closest possible control.
//
// [adjacencyCountEngines] and [oracleRun] live in degree_rewrite_test.go; the
// counters they read are process-global, so no test in this file may call
// t.Parallel.

// TestCountSubquery_HonoursInlineWhere is acceptance criterion 1.
//
// Fixture facts, established independently below: n3's :K out-edges land on n4
// and n6; only n6 carries :Q.
func TestCountSubquery_HonoursInlineWhere(t *testing.T) {
	g := degreeFixture(t, 60)
	eng, off := adjacencyCountEngines(g)

	// Pin the fixture with an enumerating form, so a case failing below is the
	// classifier's fault and not a wrong assumption about the graph.
	if got := degreeRun(t, eng, "MATCH (a:P {id: 3}) RETURN [(a)-[:K]->(b) | b.id]"); got[0] != "[4, 6]\x1f" {
		t.Fatalf("fixture assumption broken: n3's :K targets are %v, want [4, 6]", got)
	}
	if got := degreeRun(t, eng, "MATCH (a:P {id: 3}) RETURN [(a)-[:K]->(b:Q) | b.id]"); got[0] != "[6]\x1f" {
		t.Fatalf("fixture assumption broken: n3's :Q-labelled :K targets are %v, want [6]", got)
	}

	cases := []struct {
		name     string
		pattern  string // the COUNT pattern form
		fullForm string // the equivalent full subquery form
		want     string // the hand-computed absolute answer
	}{
		{
			name:     "no predicate counts every matching edge",
			pattern:  "COUNT { (a)-[:K]->(b) }",
			fullForm: "COUNT { MATCH (a)-[:K]->(b) RETURN b }",
			want:     "2\x1f",
		},
		{
			name:     "label predicate",
			pattern:  "COUNT { (a)-[:K]->(b) WHERE b:Q }",
			fullForm: "COUNT { MATCH (a)-[:K]->(b) WHERE b:Q RETURN b }",
			want:     "1\x1f",
		},
		{
			// The unambiguous case: a predicate that can never hold. Before the
			// fix this returned 2.
			name:     "a predicate that can never hold",
			pattern:  "COUNT { (a)-[:K]->(b) WHERE b.id = 999 }",
			fullForm: "COUNT { MATCH (a)-[:K]->(b) WHERE b.id = 999 RETURN b }",
			want:     "0\x1f",
		},
		{
			name:     "property equality selecting one edge",
			pattern:  "COUNT { (a)-[:K]->(b) WHERE b.id = 4 }",
			fullForm: "COUNT { MATCH (a)-[:K]->(b) WHERE b.id = 4 RETURN b }",
			want:     "1\x1f",
		},
		{
			name:     "comparison predicate",
			pattern:  "COUNT { (a)-[:K]->(b) WHERE b.id > 4 }",
			fullForm: "COUNT { MATCH (a)-[:K]->(b) WHERE b.id > 4 RETURN b }",
			want:     "1\x1f",
		},
		{
			name:     "conjunction",
			pattern:  "COUNT { (a)-[:K]->(b) WHERE b:Q AND b.id > 4 }",
			fullForm: "COUNT { MATCH (a)-[:K]->(b) WHERE b:Q AND b.id > 4 RETURN b }",
			want:     "1\x1f",
		},
		{
			name:     "negated label predicate",
			pattern:  "COUNT { (a)-[:K]->(b) WHERE NOT b:Q }",
			fullForm: "COUNT { MATCH (a)-[:K]->(b) WHERE NOT b:Q RETURN b }",
			want:     "1\x1f",
		},
		{
			name:     "predicate on an already-labelled far node",
			pattern:  "COUNT { (a)-[:K]->(b:Q) WHERE b.id > 4 }",
			fullForm: "COUNT { MATCH (a)-[:K]->(b:Q) WHERE b.id > 4 RETURN b }",
			want:     "1\x1f",
		},
		{
			name:     "untyped hop with a predicate",
			pattern:  "COUNT { (a)-->(b) WHERE b:Q }",
			fullForm: "COUNT { MATCH (a)-->(b) WHERE b:Q RETURN b }",
			want:     "1\x1f",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := degreeRun(t, eng, "MATCH (a:P {id: 3}) RETURN "+tc.pattern)
			// Oracle 1: the block form, on the arm that cannot take either
			// adjacency-answered rewrite.
			oracle := oracleRun(t, off, "MATCH (a:P {id: 3}) RETURN "+tc.fullForm)
			// Oracle 2: the pattern form itself, read by the inner plan. This is the
			// control #2242 needed — the defect was that the PATTERN form discarded
			// its predicate, so the strongest oracle is the same pattern text on a
			// path that cannot answer it from a degree.
			unrewritten := oracleRun(t, off, "MATCH (a:P {id: 3}) RETURN "+tc.pattern)
			if oracle[0] != tc.want {
				t.Fatalf("the full-subquery oracle itself returned %v, want %s — the case's "+
					"hand-computed value is wrong, not the pattern form", oracle, tc.want)
			}
			if unrewritten[0] != tc.want {
				t.Fatalf("the UNREWRITTEN reading of %s returned %v, want %s — the inner plan "+
					"is dropping the predicate, so the pattern form below is being compared "+
					"against a broken oracle", tc.pattern, unrewritten, tc.want)
			}
			if got[0] != tc.want {
				t.Errorf("%s = %v, want %s (full form agrees at %v, unrewritten pattern at %v)",
					tc.pattern, got, tc.want, oracle, unrewritten)
			}
		})
	}
}

// TestCountSubquery_InlineWhereIsRefusedByBothRecognisers is acceptance
// criterion 2. An inline WHERE is a Selection that neither the degree rewrite
// nor the labelled-hop count can evaluate, so both must decline and let the
// inner plan answer. Before #2242 gave the AST a Where field they could not even
// SEE the clause, so they answered the pattern without its predicate.
//
// It runs on a DEFAULT engine, and must keep doing so.
// [EngineOptions.DisableAdjacencyCountRewrites] would satisfy every assertion
// below for every query ever written, because the claim under test is that the
// RECOGNISERS decline — not that some engine forbade them. Moving this to the
// oracle arm would delete the test rather than harden it.
func TestCountSubquery_InlineWhereIsRefusedByBothRecognisers(t *testing.T) {
	eng := NewEngine(degreeFixture(t, 60))

	for _, q := range []string{
		// Degree-shaped but for the WHERE: unlabelled far node, single typed hop.
		"MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:K]->(b) WHERE b:Q }",
		"MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:K]->(b) WHERE b.id > 4 }",
		"MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:K]->(b) WHERE b:Q } > 0",
		// Labelled-hop-shaped but for the WHERE.
		"MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:K]->(b:Q) WHERE b.id > 4 }",
		"MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:K]->(b:Q) WHERE b.id > 4 } > 0",
	} {
		beforeDeg := degreeRewriteCount.Load()
		beforeHop := labelledHopRewriteCount.Load()
		_ = degreeRun(t, eng, q)
		if fired := degreeRewriteCount.Load() - beforeDeg; fired != 0 {
			t.Errorf("the degree rewrite fired %d time(s) for a pattern carrying an inline "+
				"WHERE it cannot evaluate:\n  %s", fired, q)
		}
		if fired := labelledHopRewriteCount.Load() - beforeHop; fired != 0 {
			t.Errorf("the labelled-hop count fired %d time(s) for a pattern carrying an "+
				"inline WHERE it cannot evaluate:\n  %s", fired, q)
		}
	}
}

// TestCountSubquery_StringRoundTripsWhere is acceptance criterion 3. A String
// that drops the clause would render the AST as a query with different meaning,
// which is how a diagnostic — or anything keyed off the rendering — would lie
// about what was asked.
//
// It round-trips through the whole query rather than a hand-built node, so it
// also asserts that the PARSER populated Where in the first place: a rendering
// can only keep a clause the parse preserved.
func TestCountSubquery_StringRoundTripsWhere(t *testing.T) {
	// The renderer parenthesises a predicate, so the expectation is the rendered
	// form rather than the source spelling — what matters is that the clause and
	// its operands survive, not that the text is byte-identical to the input.
	for _, tc := range []struct{ src, want string }{
		{"MATCH (a) RETURN COUNT { (a)-[:K]->(b) WHERE b:Q }", "WHERE (b:Q)"},
		{"MATCH (a) RETURN COUNT { (a)-[:K]->(b) WHERE b.id > 4 }", "WHERE (b.id > 4)"},
		{"MATCH (a) RETURN EXISTS { (a)-[:K]->(b) WHERE b:Q }", "WHERE (b:Q)"},
	} {
		t.Run(tc.src, func(t *testing.T) {
			q, err := parser.Parse(tc.src)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.src, err)
			}
			got := q.String()
			if !strings.Contains(got, tc.want) {
				t.Errorf("String() = %q\n  lost %q — the WHERE did not survive the parse or the rendering",
					got, tc.want)
			}
		})
	}
}
