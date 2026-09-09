package cypher

// count_subquery_body_projection_test.go — rmp #2675: a COUNT { } / EXISTS { }
// body's TRAILING projection is part of the body, and dropping it returned the
// wrong number.
//
// # What was wrong
//
// [ir.TranslateSubquery] built the inner plan from q.ReadingClauses alone and
// never translated q.Return, so the body's final projection — with its DISTINCT,
// ORDER BY, SKIP, LIMIT and any aggregation — was discarded. The counted
// quantity was therefore the PRE-projection row count. On the fixture below,
// where the anchor has exactly two outgoing :K edges:
//
//	COUNT { MATCH (a)-[:K]->(x) RETURN count(*) }   returned 2, must be 1
//	COUNT { MATCH (a)-[:K]->(x) RETURN x LIMIT 1 }  returned 2, must be 1
//	EXISTS { MATCH (z)-[:K]->(x) RETURN count(*) }  returned false, must be true
//
// A WITH was never affected: the parser appends every WITH to ReadingClauses in
// document order, so only the trailing RETURN was lost.
//
// # Where the required answers come from
//
// NOT from the openCypher TCK, which is structurally blind here: across all 220
// files in cypher/tck/features there are 13 brace-subquery occurrences and every
// one is a WHERE-position `exists { }`. There is no `COUNT { }` scenario at all,
// so `tckExecutionBaseline = 3897` stayed green over this defect for as long as
// it existed. openCypher 9 does not specify COUNT { } either — it is a
// Cypher 25 / GQL-era construct. Under CLAUDE.md's Compliance Mandate 1
// ("conformance is evidence-based: do not claim openCypher behaviour from
// memory") the authority is therefore the reference implementation's code, read
// at a named tag, plus this project's own TCK for the sub-rules it does cover.
//
// AUTHORITY 1 — what COUNT counts. github.com/neo4j/neo4j, release tag
// 2026.07.1 (commit f213380f812b820a1b312e2ea52cb3d8f1931ccc),
// community/cypher/cypher-planner/src/main/scala/org/neo4j/cypher/internal/compiler/ast/convert/plannerQuery/CreateIrExpressions.scala,
// `case countExpression @ CountExpression(q)`: the WHOLE body is converted to a
// planner query, and then either
//
//   - its final horizon is REPLACED by `AggregatingQueryProjection(count(*))` —
//     permitted only when that horizon is a `RegularQueryProjection` with
//     `QueryPagination(None, None)` and `Selections(SetExtractor())`, i.e. a
//     plain projection that cannot change the row count — with the interesting
//     order removed as well ("And also remove any ORDER BY since that won't have
//     any impact in a COUNT subquery anyway"); or
//   - for EVERY other horizon, a TAIL is appended carrying that same
//     `count(*)` over whatever the body produced.
//
// Both branches count the rows the body produces. The sibling
// CreateIrExpressionsTest.scala pins each surface: "Rewrites CountExpression"
// (plain RETURN, horizon overridden, tail None), "…with ORDER BY" (same, ORDER
// BY gone), "…with SKIP", "…with LIMIT", "…with DISTINCT" (horizon kept, tail
// appended).
//
// AUTHORITY 2 — what EXISTS tests. Same file, `case existsExpression @
// ExistsExpression(q)`: the body is converted with NO horizon override of any
// kind, and the result is an `ExistsIRExpression` over it. EXISTS is therefore
// "the body produced at least one row", evaluated over the body's own final
// projection.
//
// AUTHORITY 3 — an aggregating RETURN with no grouping key emits ONE row even
// over an empty input. This one openCypher 9 does cover, and this project's own
// TCK pins it: cypher/tck/features/clauses/return/Return4.feature, Scenario [6]
// "Keeping used expression 3" — over a graph holding a single node with no
// relationships, `MATCH p = (n)-->(b) RETURN coUnt( dIstInct p )` yields exactly
// one row, of 0. That is why an aggregating EXISTS body is TRUE rather than
// FALSE, and why COUNT of an aggregating body over a non-matching anchor is 1
// rather than 0.
//
// ORACLE — from those three, the required value of `COUNT { <body> }` is the
// number of rows `<body>` produces, and of `EXISTS { <body> }` is whether that
// number is non-zero. Every case below therefore carries its body spelled as a
// TOP-LEVEL query, and the suite measures that spelling's row count on the same
// fixture and requires the subquery to agree with it. The top-level reading
// shares no code with the subquery evaluator and is governed by the TCK, so it
// is an independent oracle rather than a restatement. The hand-computed value is
// asserted too, so a wrong oracle cannot pass either.
//
// # Every fixture discriminates, and the ones that cannot say so
//
// Each case records `dropped` — the answer the pre-fix code produced. A case
// where `want == dropped` proves nothing about this defect, so the table marks
// those explicitly and the suite asserts the marking is honest. Two surfaces
// CANNOT discriminate on an answer, by the specified semantics rather than by
// omission: a plain `RETURN <expr>` and a bare `ORDER BY` are both
// cardinality-preserving, and Neo4j deletes the ORDER BY outright. They are
// covered at the plan level instead, in
// cypher/ir/subquery_return_test.go.
//
// The counters [degreeRewriteCount] and [labelledHopRewriteCount] are
// process-global, so no test in this file may call t.Parallel.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	lpg "github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// bodyProjectionFixture builds the graph every case in this file runs on.
//
//	(a:Anchor {id: 0}) -[:K]-> (t1:Target {v: 1, ord: 2})
//	(a:Anchor {id: 0}) -[:K]-> (t2:Target {v: 1, ord: 1})
//	(z:Anchor {id: 9})                                      // no outgoing :K
//
// Three properties of it are load-bearing and each is pinned by
// TestBodyProjection_FixtureDiscriminates before any case runs:
//
//  1. the anchor's PRE-projection row count is exactly 2, so 1 and 2 are
//     different answers and a body that yields one row cannot be confused with
//     one that yields two;
//  2. t1 and t2 share `v`, so `RETURN DISTINCT x.v` has a GENUINE duplicate to
//     remove. rmp #2648 recorded that its own RETURN DISTINCT case was correct
//     only coincidentally, because its two :K targets were already distinct —
//     that mistake is not repeated here;
//  3. t1 and t2 differ in `ord`, so an ORDER BY has something to order.
//
// z exists so an aggregating body can be exercised over an EMPTY input, where
// the required answer (one row) is furthest from the dropped-projection answer
// (no rows).
func bodyProjectionFixture(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())

	addNode := func(key, label string, props map[string]int64) {
		t.Helper()
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode(%s): %v", key, err)
		}
		if err := g.SetNodeLabel(key, label); err != nil {
			t.Fatalf("SetNodeLabel(%s, %s): %v", key, label, err)
		}
		for k, v := range props {
			if err := g.SetNodeProperty(key, k, lpg.Int64Value(v)); err != nil {
				t.Fatalf("SetNodeProperty(%s, %s): %v", key, k, err)
			}
		}
	}

	addNode("a", "Anchor", map[string]int64{"id": 0})
	addNode("z", "Anchor", map[string]int64{"id": 9})
	addNode("t1", "Target", map[string]int64{"v": 1, "ord": 2})
	addNode("t2", "Target", map[string]int64{"v": 1, "ord": 1})

	for _, dst := range []string{"t1", "t2"} {
		if err := g.AddEdge("a", dst, 1); err != nil {
			t.Fatalf("AddEdge(a, %s): %v", dst, err)
		}
		g.SetEdgeLabel("a", dst, "K")
	}
	return g
}

// bodyProjectionCase is one body, read three ways: as a subquery, as the
// top-level query it is sugar for, and as a hand-computed number.
type bodyProjectionCase struct {
	name string
	// subquery is the full query whose single row carries the COUNT/EXISTS.
	subquery string
	// topLevel spells the SAME body as an ordinary query, so its row count is
	// the quantity the reference implementation says COUNT returns. It is nil
	// only where no equivalent spelling exists.
	topLevel string
	// want is the hand-computed answer, rendered as [degreeRun] renders it.
	want string
	// wantRows is the number of rows topLevel must produce. Asserted separately
	// so a wrong oracle fails loudly instead of agreeing with a wrong answer.
	wantRows int
	// dropped is the answer the pre-fix code produced — the pre-projection
	// reading. When it equals want the case cannot discriminate, and
	// nonDiscriminating must say so.
	dropped string
	// nonDiscriminating marks a case kept for coverage even though the right and
	// the wrong answer coincide. The suite asserts this flag against want vs
	// dropped, so it cannot drift.
	nonDiscriminating bool
	why               string
}

func bodyProjectionCases() []bodyProjectionCase {
	return []bodyProjectionCase{
		// ── COUNT ────────────────────────────────────────────────────────────
		{
			name:     "COUNT, RETURN that aggregates",
			subquery: "MATCH (a:Anchor {id: 0}) RETURN COUNT { MATCH (a)-[:K]->(x) RETURN count(*) }",
			topLevel: "MATCH (a:Anchor {id: 0}) MATCH (a)-[:K]->(x) RETURN count(*)",
			want:     "1\x1f", wantRows: 1, dropped: "2\x1f",
			why: "count(*) with no grouping key collapses the two matches into one row, and it is " +
				"that row the COUNT counts (CreateIrExpressions: an aggregating horizon takes the " +
				"tail branch)",
		},
		{
			name:     "COUNT, RETURN with a LIMIT",
			subquery: "MATCH (a:Anchor {id: 0}) RETURN COUNT { MATCH (a)-[:K]->(x) RETURN x LIMIT 1 }",
			topLevel: "MATCH (a:Anchor {id: 0}) MATCH (a)-[:K]->(x) RETURN x LIMIT 1",
			want:     "1\x1f", wantRows: 1, dropped: "2\x1f",
			why: "a horizon with pagination is kept and the count(*) is appended as a tail above it " +
				"(CreateIrExpressionsTest: \"Rewrites CountExpression with LIMIT\")",
		},
		{
			name:     "COUNT, RETURN with a SKIP",
			subquery: "MATCH (a:Anchor {id: 0}) RETURN COUNT { MATCH (a)-[:K]->(x) RETURN x SKIP 1 }",
			topLevel: "MATCH (a:Anchor {id: 0}) MATCH (a)-[:K]->(x) RETURN x SKIP 1",
			want:     "1\x1f", wantRows: 1, dropped: "2\x1f",
			why: "SKIP is the other half of QueryPagination and takes the same tail branch " +
				"(CreateIrExpressionsTest: \"Rewrites CountExpression with SKIP\")",
		},
		{
			name:     "COUNT, RETURN DISTINCT over a genuine duplicate",
			subquery: "MATCH (a:Anchor {id: 0}) RETURN COUNT { MATCH (a)-[:K]->(x) RETURN DISTINCT x.v }",
			topLevel: "MATCH (a:Anchor {id: 0}) MATCH (a)-[:K]->(x) RETURN DISTINCT x.v",
			want:     "1\x1f", wantRows: 1, dropped: "2\x1f",
			why: "t1.v = t2.v = 1, so DISTINCT really removes a row here — the case rmp #2648 " +
				"could not make, because its two targets were already distinct",
		},
		{
			name:     "COUNT, ORDER BY with a LIMIT",
			subquery: "MATCH (a:Anchor {id: 0}) RETURN COUNT { MATCH (a)-[:K]->(x) RETURN x ORDER BY x.ord LIMIT 1 }",
			topLevel: "MATCH (a:Anchor {id: 0}) MATCH (a)-[:K]->(x) RETURN x ORDER BY x.ord LIMIT 1",
			want:     "1\x1f", wantRows: 1, dropped: "2\x1f",
			why: "the ordered-and-truncated form, which GoGraph fuses into a Top; the discrimination " +
				"comes from the LIMIT, because a bare ORDER BY cannot change a row count at all",
		},
		{
			name:     "COUNT, aggregating RETURN with a grouping key",
			subquery: "MATCH (a:Anchor {id: 0}) RETURN COUNT { MATCH (a)-[:K]->(x) RETURN x.v, count(*) }",
			topLevel: "MATCH (a:Anchor {id: 0}) MATCH (a)-[:K]->(x) RETURN x.v, count(*)",
			want:     "1\x1f", wantRows: 1, dropped: "2\x1f",
			why: "both targets share v, so the two matches group into ONE row; a grouping key does " +
				"not exempt the horizon from the tail branch",
		},
		{
			name:     "COUNT, aggregating RETURN over an EMPTY body",
			subquery: "MATCH (z:Anchor {id: 9}) RETURN COUNT { MATCH (z)-[:K]->(x) RETURN count(*) }",
			topLevel: "MATCH (z:Anchor {id: 9}) MATCH (z)-[:K]->(x) RETURN count(*)",
			want:     "1\x1f", wantRows: 1, dropped: "0\x1f",
			why: "z has no outgoing :K, yet an aggregation with no grouping key still emits one row " +
				"(TCK Return4.feature [6]) — so the count is 1 and not 0. This is the widest gap " +
				"between the right answer and the dropped-projection one in the file",
		},
		{
			name:     "COUNT, plain RETURN",
			subquery: "MATCH (a:Anchor {id: 0}) RETURN COUNT { MATCH (a)-[:K]->(x) RETURN x }",
			topLevel: "MATCH (a:Anchor {id: 0}) MATCH (a)-[:K]->(x) RETURN x",
			want:     "2\x1f", wantRows: 2, dropped: "2\x1f", nonDiscriminating: true,
			why: "a plain projection is cardinality-preserving, and Neo4j OVERRIDES such a horizon " +
				"with count(*) rather than appending a tail — so the two readings agree BY " +
				"SPECIFICATION. Discriminated at the plan level instead " +
				"(cypher/ir/subquery_return_test.go)",
		},
		{
			name:     "COUNT, bare ORDER BY",
			subquery: "MATCH (a:Anchor {id: 0}) RETURN COUNT { MATCH (a)-[:K]->(x) RETURN x ORDER BY x.ord }",
			topLevel: "MATCH (a:Anchor {id: 0}) MATCH (a)-[:K]->(x) RETURN x ORDER BY x.ord",
			want:     "2\x1f", wantRows: 2, dropped: "2\x1f", nonDiscriminating: true,
			why: "ordering cannot change how many rows there are, and Neo4j removes the ORDER BY " +
				"outright for a COUNT body. Discriminated at the plan level instead",
		},

		// ── EXISTS, on the same path ─────────────────────────────────────────
		{
			name:     "EXISTS, RETURN that aggregates over an EMPTY body",
			subquery: "MATCH (z:Anchor {id: 9}) RETURN EXISTS { MATCH (z)-[:K]->(x) RETURN count(*) }",
			topLevel: "MATCH (z:Anchor {id: 9}) MATCH (z)-[:K]->(x) RETURN count(*)",
			want:     "true\x1f", wantRows: 1, dropped: "false\x1f",
			why: "ExistsExpression converts the body with no override, so EXISTS asks whether the " +
				"BODY produced a row — and an aggregation with no grouping key produces one even " +
				"over an empty input. This is the case the task singles out: a dropped LIMIT is " +
				"invisible to an existence test, a dropped aggregating RETURN is not",
		},
		{
			name:     "EXISTS, RETURN with LIMIT 0",
			subquery: "MATCH (a:Anchor {id: 0}) RETURN EXISTS { MATCH (a)-[:K]->(x) RETURN x LIMIT 0 }",
			topLevel: "MATCH (a:Anchor {id: 0}) MATCH (a)-[:K]->(x) RETURN x LIMIT 0",
			want:     "false\x1f", wantRows: 0, dropped: "true\x1f",
			why: "LIMIT 0 truncates the body to no rows, so the existence test must fail even though " +
				"the pattern matches twice",
		},
		{
			name:     "EXISTS, RETURN with a SKIP past the end",
			subquery: "MATCH (a:Anchor {id: 0}) RETURN EXISTS { MATCH (a)-[:K]->(x) RETURN x SKIP 2 }",
			topLevel: "MATCH (a:Anchor {id: 0}) MATCH (a)-[:K]->(x) RETURN x SKIP 2",
			want:     "false\x1f", wantRows: 0, dropped: "true\x1f",
			why: "SKIP 2 consumes both matches, so the body is empty and the existence test fails",
		},
		{
			name:     "EXISTS, plain RETURN",
			subquery: "MATCH (a:Anchor {id: 0}) RETURN EXISTS { MATCH (a)-[:K]->(x) RETURN x }",
			topLevel: "MATCH (a:Anchor {id: 0}) MATCH (a)-[:K]->(x) RETURN x",
			want:     "true\x1f", wantRows: 2, dropped: "true\x1f", nonDiscriminating: true,
			why: "the control: the shape the TCK's own RETURN-bearing EXISTS bodies have " +
				"(ExistentialSubquery2 [1]), which must keep answering as it always did",
		},
	}
}

// TestBodyProjection_FixtureDiscriminates pins every assumption the table above
// rests on, BEFORE any case runs.
//
// A hand-computed value that is wrong because the fixture is not what the
// comment says would otherwise be blamed on the translation. In particular, if
// the anchor's pre-projection row count were 1 rather than 2, then every "want
// 1" case would agree with the dropped-projection answer and the whole suite
// would be vacuous while staying green.
func TestBodyProjection_FixtureDiscriminates(t *testing.T) {
	g := bodyProjectionFixture(t)
	eng := NewEngine(g)

	preProjection := degreeRun(t, eng, "MATCH (a:Anchor {id: 0}) MATCH (a)-[:K]->(x) RETURN x")
	if len(preProjection) != 2 {
		t.Fatalf("the anchor's PRE-projection row count is %d, want 2. Every \"want 1\" case in "+
			"this file needs 1 and 2 to be different answers; they are not, so the suite would "+
			"be vacuous.", len(preProjection))
	}

	dup := degreeRun(t, eng, "MATCH (a:Anchor {id: 0}) MATCH (a)-[:K]->(x) RETURN DISTINCT x.v")
	if len(dup) != 1 {
		t.Fatalf("RETURN DISTINCT x.v over the two targets produced %d row(s), want 1. The two "+
			"targets do not share v, so the DISTINCT case would be correct only coincidentally "+
			"— which is exactly the mistake rmp #2648 recorded.", len(dup))
	}

	ord := degreeRun(t, eng, "MATCH (a:Anchor {id: 0}) MATCH (a)-[:K]->(x) RETURN x.ord ORDER BY x.ord")
	if len(ord) != 2 || ord[0] == ord[1] {
		t.Fatalf("the two targets' ord values are %v; they must differ for an ORDER BY to have "+
			"anything to order", ord)
	}

	empty := degreeRun(t, eng, "MATCH (z:Anchor {id: 9}) MATCH (z)-[:K]->(x) RETURN x")
	if len(empty) != 0 {
		t.Fatalf("z produced %d :K match(es), want 0 — the empty-body cases need an anchor with "+
			"no outgoing :K", len(empty))
	}
}

// TestBodyProjection_SubqueryAnswersMatchTheBody is the answer-level half of
// rmp #2675.
//
// Each case is read three ways and all three must agree:
//
//  1. the subquery, on a default engine — the path under test;
//  2. the same body spelled as a TOP-LEVEL query, whose row count is what the
//     reference implementation says the COUNT returns and what the EXISTS tests.
//     It shares no code with the subquery evaluator;
//  3. the hand-computed value, which shares no code with either.
//
// The case also asserts that `dropped` — the pre-fix answer — really is
// different from `want` wherever it claims to be, so a case cannot silently
// decay into one that proves nothing.
func TestBodyProjection_SubqueryAnswersMatchTheBody(t *testing.T) {
	g := bodyProjectionFixture(t)
	on, off := adjacencyCountEngines(g)

	for _, tc := range bodyProjectionCases() {
		t.Run(tc.name, func(t *testing.T) {
			// The case's own honesty check: `nonDiscriminating` must agree with
			// what want and dropped actually say.
			if (tc.want == tc.dropped) != tc.nonDiscriminating {
				t.Fatalf("the case's nonDiscriminating flag is %v but want=%q and dropped=%q. "+
					"A case where the right and the wrong answer coincide proves nothing about "+
					"this defect and must say so.", tc.nonDiscriminating, tc.want, tc.dropped)
			}

			// Oracle 2: the body as an ordinary query.
			bodyRows := degreeRun(t, on, tc.topLevel)
			if len(bodyRows) != tc.wantRows {
				t.Fatalf("the body spelled as a top-level query produced %d row(s), want %d. The "+
					"oracle is wrong, so every comparison against it below is worthless.\n"+
					"  body: %s", len(bodyRows), tc.wantRows, tc.topLevel)
			}

			got := degreeRun(t, on, tc.subquery)
			if len(got) != 1 {
				t.Fatalf("the query returned %d rows, want exactly 1 carrying the subquery's "+
					"value: %s", len(got), tc.subquery)
			}

			// Oracle 3: the hand-computed value.
			if got[0] != tc.want {
				t.Errorf("got %q, want %q — the subquery does not agree with its own body.\n"+
					"  query: %s\n  body as a query: %s (%d row(s))\n"+
					"  pre-fix answer was %q\n  why: %s",
					got[0], tc.want, tc.subquery, tc.topLevel, len(bodyRows), tc.dropped, tc.why)
			}

			// The answer must not depend on the access path: the same query read
			// by an engine that cannot take either adjacency-answered rewrite has
			// to agree.
			unrewritten := oracleRun(t, off, tc.subquery)
			assertRowsEqual(t, got, unrewritten,
				"the subquery disagrees with its unrewritten reading", tc.subquery)
		})
	}
}

// TestBodyProjection_NoAdjacencyRewriteAnswersTheseBodies guards the suite above
// against being answered by the wrong machinery.
//
// Every body in the table carries a trailing RETURN, and [ir.PatternFormOf]
// refuses every RETURN-bearing body (rmp #2648), so neither the degree rewrite
// nor the labelled single-hop count may fire for any of them. If a future change
// widened that boundary without teaching the recognisers about the horizon, the
// answers would silently revert to the pre-projection count — which is the very
// defect this file exists to prevent. That failure would show up in the answers
// too, but here it names its cause.
func TestBodyProjection_NoAdjacencyRewriteAnswersTheseBodies(t *testing.T) {
	g := bodyProjectionFixture(t)
	on, _ := adjacencyCountEngines(g)

	for _, tc := range bodyProjectionCases() {
		t.Run(tc.name, func(t *testing.T) {
			beforeDeg := degreeRewriteCount.Load()
			beforeHop := labelledHopRewriteCount.Load()
			_ = degreeRun(t, on, tc.subquery)
			if fired := degreeRewriteCount.Load() - beforeDeg; fired != 0 {
				t.Errorf("the DEGREE rewrite fired %d time(s) for a RETURN-bearing body; that "+
					"rewrite answers from the adjacency and cannot honour a horizon.\n  query: %s",
					fired, tc.subquery)
			}
			if fired := labelledHopRewriteCount.Load() - beforeHop; fired != 0 {
				t.Errorf("the LABELLED SINGLE-HOP count fired %d time(s) for a RETURN-bearing "+
					"body; same reason.\n  query: %s", fired, tc.subquery)
			}
		})
	}
}

// TestBodyProjection_CorrelationSurvivesTheProjection is the correlation control.
//
// Translating the body's RETURN layers a Projection (and, per case, a Distinct,
// Sort, Skip, Limit, Top or EagerAggregation) on top of the inner pipeline whose
// leaf is the seed Argument. If any of those operators displaced or shadowed the
// correlation columns, a correlated subquery would stop varying with the outer
// row — and would do so silently, by answering some constant.
//
// This drives the SAME body from two anchors with different degrees and requires
// the answers to differ, so a constant cannot pass.
func TestBodyProjection_CorrelationSurvivesTheProjection(t *testing.T) {
	g := bodyProjectionFixture(t)
	on, _ := adjacencyCountEngines(g)

	rows := degreeRun(t, on,
		"MATCH (n:Anchor) RETURN n.id, COUNT { MATCH (n)-[:K]->(x) RETURN x }, "+
			"COUNT { MATCH (n)-[:K]->(x) RETURN count(*) } ORDER BY n.id")
	if len(rows) != 2 {
		t.Fatalf("expected one row per :Anchor node (2), got %d: %v", len(rows), rows)
	}
	// a (id 0) has two :K edges; z (id 9) has none. The plain-RETURN column must
	// follow the degree, and the aggregating column must be 1 for BOTH — an
	// aggregation with no grouping key emits one row whatever the input.
	if want := "0\x1f2\x1f1\x1f"; rows[0] != want {
		t.Errorf("row for the two-edge anchor is %q, want %q", rows[0], want)
	}
	if want := "9\x1f0\x1f1\x1f"; rows[1] != want {
		t.Errorf("row for the zero-edge anchor is %q, want %q — if this reads 0 in the last "+
			"column the aggregating body is not being evaluated; if it reads 2 the subquery has "+
			"stopped being correlated", rows[1], want)
	}
}

// TestBodyProjection_NestedSubqueryInsideTheBodysReturn exercises the one shape
// the fix newly makes reachable: an expression in the body's RETURN that is
// itself a subquery.
//
// Before the fix the body's RETURN was never translated, so a subquery written
// inside it was never compiled and never evaluated. It now is, and it must be
// correlated with the INNER row rather than the outer one.
//
// The assertion is deliberately built so that a nested subquery returning a
// CONSTANT would fail it. The nested count is 1 for b and 0 for c and d, so
// DISTINCT over the three projected values leaves two rows; a constant would
// leave one, and a dropped projection would leave three. All three outcomes are
// distinguishable, which is what makes this a test of the nested evaluation and
// not only of the projection.
func TestBodyProjection_NestedSubqueryInsideTheBodysReturn(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	for i, key := range []string{"a", "b", "c", "d"} {
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode(%s): %v", key, err)
		}
		if err := g.SetNodeLabel(key, "N"); err != nil {
			t.Fatalf("SetNodeLabel(%s): %v", key, err)
		}
		if err := g.SetNodeProperty(key, "id", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty(%s): %v", key, err)
		}
	}
	// a has three :K targets; of those only b has one of its own.
	for _, e := range [][2]string{{"a", "b"}, {"a", "c"}, {"a", "d"}, {"b", "c"}} {
		if err := g.AddEdge(e[0], e[1], 1); err != nil {
			t.Fatalf("AddEdge(%s, %s): %v", e[0], e[1], err)
		}
		g.SetEdgeLabel(e[0], e[1], "K")
	}
	on, off := adjacencyCountEngines(g)

	// Pin the fixture before trusting the case: three targets, and their nested
	// counts really are {1, 0, 0}.
	if got := degreeRun(t, on, "MATCH (a:N {id: 0}) MATCH (a)-[:K]->(x) RETURN x.id ORDER BY x.id"); len(got) != 3 {
		t.Fatalf("the anchor has %d :K targets, want 3 — the case below needs 2 and 3 to differ: %v", len(got), got)
	}
	nested := degreeRun(t, on,
		"MATCH (a:N {id: 0}) MATCH (a)-[:K]->(x) RETURN COUNT { MATCH (x)-[:K]->(y) RETURN y } ORDER BY x.id")
	if len(nested) != 3 || nested[0] != "1\x1f" || nested[1] != "0\x1f" || nested[2] != "0\x1f" {
		t.Fatalf("the per-target nested counts are %v, want [1 0 0]; without two distinct values "+
			"the DISTINCT below cannot tell a real nested evaluation from a constant", nested)
	}

	const q = "MATCH (a:N {id: 0}) RETURN COUNT { MATCH (a)-[:K]->(x) " +
		"RETURN DISTINCT COUNT { MATCH (x)-[:K]->(y) RETURN y } }"
	got := degreeRun(t, on, q)
	if len(got) != 1 || got[0] != "2\x1f" {
		t.Errorf("got %v, want [\"2\\x1f\"].\n"+
			"  3 means the body's RETURN — and its DISTINCT — was dropped (rmp #2675);\n"+
			"  1 means the nested subquery returned the same value for every target, so it is "+
			"not correlated with the inner row;\n"+
			"  2 is the three projected values {1, 0, 0} deduplicated.\n  query: %s", got, q)
	}
	assertRowsEqual(t, got, oracleRun(t, off, q),
		"the nested form disagrees with its unrewritten reading", q)
}
