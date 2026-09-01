package cypher

// count_subquery_block_form_test.go — the suite for rmp #2648: a single-MATCH
// BLOCK-form subquery body reaches the same adjacency-answered fast paths as the
// pattern form, and nothing wider than that does.
//
// # What was wrong
//
// [ast.CountSubquery] and [ast.ExistsSubquery] carry one question in two shapes:
// `COUNT { (a)-[:K]->(:Q) }` fills Pattern, `COUNT { MATCH (a)-[:K]->(:Q) }`
// fills Query and leaves Pattern nil. Every dispatch site read Pattern, and both
// recognisers reject a nil pattern on their first line, so the second spelling
// was STRUCTURALLY unable to reach either rewrite — it drove a compiled inner
// plan once per outer row for a question the adjacency answers in one walk.
//
// # The obligations, in the order they matter
//
//  1. THE TARGET SHAPE ARRIVES. A block form that is exactly the sugar-free
//     spelling of a rewritable pattern must move the right counter, and must
//     agree with three oracles that share no code with it.
//  2. NOTHING WIDER ARRIVES. Every block body outside the boundary must move
//     NEITHER counter. Two of those cases — OPTIONAL MATCH, and a RETURN that
//     aggregates — would be WRONG ANSWERS if admitted, not merely different
//     access paths, so this obligation is correctness and not conservatism.
//  3. THE NORMALISATION IS THE EXACT INVERSE OF THE DESUGARING. The boundary is
//     not a hand-drawn line: it is the set of bodies that
//     [countToSingleQuery] / [existsToSingleQuery] could have BUILT from a
//     pattern form. TestPatternFormOf_IsInverseOfDesugaring asserts the round
//     trip, which is what makes obligation 1 semantics-preserving by
//     construction rather than by argument.
//
// [adjacencyCountEngines], [oracleRun] and [degreeRun] live in
// degree_rewrite_test.go. The counters they read are process-global, so no test
// in this file may call t.Parallel.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
)

// TestBlockFormNormalisation_ReachesTheAdjacencyRewrites is obligation 1.
//
// Each case runs FOUR readings of one question and requires all four to agree:
//
//  1. the block form on the DEFAULT arm — the path under test, which must move
//     the counter the case names and must not move the other one;
//  2. the block form on the REWRITE-FREE arm, so the same text is also read by
//     an inner plan ([oracleRun] asserts neither rewrite fired);
//  3. the PATTERN form on the rewrite-free arm, a second, independently
//     translated spelling;
//  4. want, the hand-computed absolute answer, which shares no code at all.
//
// Reading 4 is not decoration. The lesson recorded in [degreeDifferential] is
// that two forms agreeing proves nothing when both go through the same
// comparison code, and after this change the two SPELLINGS deliberately share
// the whole path — which is precisely what makes a differential between them
// weaker, not stronger, than it used to be.
func TestBlockFormNormalisation_ReachesTheAdjacencyRewrites(t *testing.T) {
	g := degreeFixture(t, 60)
	on, off := adjacencyCountEngines(g)

	// Pin the fixture with an enumerating form, so a wrong hand-computed value
	// below is caught here as a broken assumption rather than blamed on the
	// normalisation. n3's out-edges are n4 (:K), n5 (:M), n6 (:K); of those only
	// n6 carries :Q.
	if got := degreeRun(t, on, "MATCH (a:P {id: 3}) RETURN [(a)-[:K]->(b) | b.id]"); got[0] != "[4, 6]\x1f" {
		t.Fatalf("fixture assumption broken: n3's :K targets are %v, want [4, 6]", got)
	}
	if got := degreeRun(t, on, "MATCH (a:P {id: 3}) RETURN [(a)-[:K]->(b:Q) | b.id]"); got[0] != "[6]\x1f" {
		t.Fatalf("fixture assumption broken: n3's :Q-labelled :K targets are %v, want [6]", got)
	}

	cases := []struct {
		name       string
		block      string
		pattern    string
		want       string
		wantDegree bool
		wantHop    bool
	}{
		{
			name:    "typed hop to a labelled far node",
			block:   "MATCH (a:P {id: 3}) RETURN COUNT { MATCH (a)-[:K]->(:Q) }",
			pattern: "MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:K]->(:Q) }",
			want:    "1\x1f",
			wantHop: true,
		},
		{
			name:    "untyped hop to a labelled far node",
			block:   "MATCH (a:P {id: 3}) RETURN COUNT { MATCH (a)-->(:Q) }",
			pattern: "MATCH (a:P {id: 3}) RETURN COUNT { (a)-->(:Q) }",
			want:    "1\x1f",
			wantHop: true,
		},
		{
			name:       "typed degree, anonymous far node",
			block:      "MATCH (a:P {id: 3}) RETURN COUNT { MATCH (a)-[:K]->() }",
			pattern:    "MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:K]->() }",
			want:       "2\x1f",
			wantDegree: true,
		},
		{
			name:       "untyped degree",
			block:      "MATCH (a:P {id: 3}) RETURN COUNT { MATCH (a)-->() }",
			pattern:    "MATCH (a:P {id: 3}) RETURN COUNT { (a)-->() }",
			want:       "3\x1f",
			wantDegree: true,
		},
		{
			// The far node NAMES a variable, but one the subquery introduces
			// rather than one the outer row binds, so it is still a degree. This
			// is the clause [ir.DegreeCountableShape] spells with
			// [ir.UserNamed], and it must decide the same way for both spellings.
			name:       "far node introduces its own variable",
			block:      "MATCH (a:P {id: 3}) RETURN COUNT { MATCH (a)-[:K]->(x) }",
			pattern:    "MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:K]->(x) }",
			want:       "2\x1f",
			wantDegree: true,
		},
		{
			// EXISTS in RETURN position, not WHERE position: a WHERE-position
			// EXISTS is planned as a SemiApply operator rather than evaluated as
			// an expression, so it never reaches the evaluator at all.
			name:    "EXISTS over a labelled hop",
			block:   "MATCH (a:P {id: 3}) RETURN EXISTS { MATCH (a)-[:K]->(:Q) }",
			pattern: "MATCH (a:P {id: 3}) RETURN EXISTS { (a)-[:K]->(:Q) }",
			want:    "true\x1f",
			wantHop: true,
		},
		{
			// The BOUNDED entry point (EvalCountBounded), where the cap and the
			// short-circuit live. A comparison against a literal takes a
			// different dispatch site from the two above.
			name:       "bounded comparison over the whole graph",
			block:      "MATCH (a:P) WHERE COUNT { MATCH (a)-[:K]->() } > 0 RETURN count(a)",
			pattern:    "MATCH (a:P) WHERE COUNT { (a)-[:K]->() } > 0 RETURN count(a)",
			want:       "45\x1f",
			wantDegree: true,
		},
		{
			name:    "bounded comparison over a labelled hop",
			block:   "MATCH (a:P) WHERE COUNT { MATCH (a)-[:K]->(:Q) } > 0 RETURN count(a)",
			pattern: "MATCH (a:P) WHERE COUNT { (a)-[:K]->(:Q) } > 0 RETURN count(a)",
			wantHop: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			beforeDeg := degreeRewriteCount.Load()
			beforeHop := labelledHopRewriteCount.Load()
			got := degreeRun(t, on, tc.block)
			deg := degreeRewriteCount.Load() - beforeDeg
			hop := labelledHopRewriteCount.Load() - beforeHop

			// Obligation 1: the counter the case names must MOVE. Without this
			// half every assertion below is satisfied by a build in which the
			// normalisation does nothing at all.
			if tc.wantDegree && deg == 0 {
				t.Fatalf("the DEGREE rewrite did not fire for the block form, so it is still "+
					"unreachable from this spelling and the rest of this case proves nothing.\n  query: %s", tc.block)
			}
			if tc.wantHop && hop == 0 {
				t.Fatalf("the LABELLED SINGLE-HOP count did not fire for the block form, so it is "+
					"still unreachable from this spelling and the rest of this case proves nothing.\n  query: %s", tc.block)
			}
			// And the OTHER counter must not: which recogniser serves a shape is
			// part of the claim, and a case that quietly migrated between them
			// would leave one of them untested while still passing.
			if !tc.wantDegree && deg != 0 {
				t.Errorf("the degree rewrite fired %d time(s) for a shape this case attributes to the "+
					"labelled single-hop count; a labelled far node must stay ineligible for a degree.\n  query: %s", deg, tc.block)
			}
			if !tc.wantHop && hop != 0 {
				t.Errorf("the labelled single-hop count fired %d time(s) for a shape this case "+
					"attributes to the degree rewrite.\n  query: %s", hop, tc.block)
			}

			// Oracle 1: the SAME text, unrewritten.
			unrewritten := oracleRun(t, off, tc.block)
			assertRowsEqual(t, got, unrewritten,
				"the block form disagrees with its own unrewritten reading", tc.block)

			// Oracle 2: the pattern-form spelling, also unrewritten. This is the
			// question the normalisation claims the block form is equal to, read
			// by a path that cannot have taken the normalisation.
			patternUnrewritten := oracleRun(t, off, tc.pattern)
			assertRowsEqual(t, got, patternUnrewritten,
				"the block form disagrees with the unrewritten reading of its pattern-form twin", tc.pattern)

			// Oracle 3: the pattern form on the DEFAULT arm. Both spellings now
			// take the same route, so this is the weakest of the four — it is here
			// because a divergence between them would mean the normalisation
			// produced a DIFFERENT pattern rather than the same one.
			patternRewritten := degreeRun(t, on, tc.pattern)
			assertRowsEqual(t, got, patternRewritten,
				"the two spellings disagree on the fast path", tc.pattern)

			// Oracle 4: the hand-computed value, which shares no code.
			if tc.want != "" && (len(got) == 0 || got[0] != tc.want) {
				t.Errorf("absolute value is wrong: got %v, want %q — every agreement above means "+
					"nothing if the shared path is broken.\n  query: %s", got, tc.want, tc.block)
			}
		})
	}
}

// TestBlockFormNormalisation_RefusedShapes is obligation 2: the guard fires for
// the admitted bodies and for NOTHING else.
//
// It runs on a DEFAULT engine, and must keep doing so. The claim is that the
// NORMALISATION refuses each body; an engine built with
// [EngineOptions.DisableAdjacencyCountRewrites] would satisfy the counter
// assertions for every query ever written, so moving this to the oracle arm
// would delete the test rather than harden it. The oracle arm appears only as
// the answer check, where it belongs.
//
// # Two of these are correctness, not caution
//
//   - OPTIONAL MATCH emits one row of NULLs when nothing matches, so its count
//     is 1 where a degree says 0. Neo4j refuses it too, through
//     QuerySolvableByGetDegree requiring the query graph's optionalMatches to be
//     empty.
//   - A RETURN that AGGREGATES collapses every match into one row, so the count
//     is 1 whatever the degree is. Neo4j refuses that as well: an aggregating
//     horizon sends CreateIrExpressions down its `case _` branch, which appends
//     a TAIL, and QuerySolvableByGetDegree requires `tail = None`.
//
// The rest are the boundary itself. `RETURN <non-aggregating item>` is the one
// case where GoGraph is deliberately NARROWER than Neo4j, which admits it by
// replacing the projection with count(*) inside a planner that owns the whole
// query. See [ir.PatternFormOf] for why that is not reproduced here.
//
// # Read the hand-computed values with one caveat
//
// A body's FINAL projection is never translated: [ir.TranslateSubquery] builds
// the inner plan from q.ReadingClauses alone. So a RETURN's DISTINCT, ORDER BY,
// SKIP, LIMIT or aggregation is discarded on the inner-plan path too, and two
// cases below therefore assert no absolute value — the value GoGraph produces
// for them is not the value openCypher requires. That is a PRE-EXISTING defect,
// invisible to the openCypher TCK (which has no `COUNT { }` scenario at all),
// untouched by this change, and out of its scope; each case carries the detail.
// Where a value IS asserted it is the correct one, but for two of them only
// because the discarded clause happens to be unobservable on this fixture.
func TestBlockFormNormalisation_RefusedShapes(t *testing.T) {
	g := degreeFixture(t, 60)
	on, off := adjacencyCountEngines(g)

	cases := []struct {
		name string
		q    string
		why  string
		want string
	}{
		{
			name: "RETURN of a plain item",
			q:    "MATCH (a:P {id: 3}) RETURN COUNT { MATCH (a)-[:K]->(:Q) RETURN 1 }",
			why:  "a horizon can change how many rows reach the count; GoGraph is narrower than Neo4j here by choice",
			want: "1\x1f",
		},
		{
			name: "RETURN of the far node — the spelling every other oracle in this package uses",
			q:    "MATCH (a:P {id: 3}) RETURN COUNT { MATCH (a)-[:K]->(x) RETURN x }",
			why:  "degree_rewrite_test.go and count_subquery_where_test.go both use this as a control; admitting it would vacate them",
			want: "2\x1f",
		},
		{
			name: "RETURN that aggregates",
			q:    "MATCH (a:P {id: 3}) RETURN COUNT { MATCH (a)-[:K]->(x) RETURN count(*) }",
			why:  "count(*) collapses two matches into one row, so a degree of 2 is not the answer",
			// NO hand-computed value here, deliberately, and this is a FINDING
			// rather than a gap in the case.
			//
			// openCypher's COUNT { … } counts the rows the BODY returns, and
			// `MATCH (a)-[:K]->(x) RETURN count(*)` returns exactly one row, so the
			// answer is 1. GoGraph returns 2 — the two matches — because
			// [ir.TranslateSubquery] builds the inner plan from q.ReadingClauses
			// ALONE and never translates q.Return, so the body's final projection
			// and everything attached to it (DISTINCT, ORDER BY, SKIP, LIMIT, an
			// aggregation) is discarded. A WITH is unaffected: the parser embeds
			// WITH clauses in ReadingClauses for a multi-part query, which is why
			// the WITH case below answers correctly.
			//
			// That defect is PRE-EXISTING and untouched by rmp #2648: this body is
			// refused by the normalisation, so it keeps taking exactly the inner
			// plan it always took. Fixing it changes answers and belongs to its own
			// task, so no value is asserted here rather than enshrining a wrong
			// one. The openCypher TCK cannot catch it — it has ZERO `COUNT { }`
			// scenarios; all 13 brace-subquery occurrences in
			// cypher/tck/features are WHERE-position `exists { }`.
			//
			// It is also the strongest argument for the boundary [ir.PatternFormOf]
			// draws: admitting Neo4j's wider RETURN-bearing set would have meant
			// normalising away a horizon the engine ALSO ignores.
		},
		{
			name: "OPTIONAL MATCH",
			q:    "MATCH (a:P {id: 0}) RETURN COUNT { OPTIONAL MATCH (a)-[:K]->(:Q) }",
			why:  "OPTIONAL MATCH emits a row of nulls when nothing matches; n0 has degree 0, so a degree would say 0",
			// No hand-computed value: the point of the case is the counters and
			// the agreement with the unrewritten reading, not pinning GoGraph's
			// OPTIONAL-in-subquery cardinality, which this task did not change.
		},
		{
			name: "two MATCH clauses",
			q:    "MATCH (a:P {id: 3}) RETURN COUNT { MATCH (a)-[:K]->(:Q) MATCH (a)-[:M]->(x) }",
			why:  "two reading clauses are a join; one adjacency walk cannot answer it",
		},
		{
			name: "UNWIND before the MATCH",
			q:    "MATCH (a:P {id: 3}) RETURN COUNT { UNWIND [1, 2] AS u MATCH (a)-[:K]->(:Q) }",
			why:  "the UNWIND multiplies the rows the MATCH produces, so the count is not the neighbour count",
		},
		{
			name: "WHERE in a block body, degree-shaped otherwise",
			q:    "MATCH (a:P {id: 3}) RETURN COUNT { MATCH (a)-[:K]->(b) WHERE b:Q }",
			why:  "a WHERE is a Selection neither recogniser can evaluate; refused exactly as the pattern form's inline WHERE is (rmp #2242)",
			want: "1\x1f",
		},
		{
			name: "WHERE in a block body, labelled-hop-shaped otherwise",
			q:    "MATCH (a:P {id: 3}) RETURN COUNT { MATCH (a)-[:K]->(b:Q) WHERE b.id > 4 }",
			why:  "same Selection rule, on the shape the labelled-hop recogniser would otherwise take",
			want: "1\x1f",
		},
		{
			name: "WHERE in a block EXISTS body",
			q:    "MATCH (a:P {id: 3}) RETURN EXISTS { MATCH (a)-[:K]->(b) WHERE b:Q }",
			why:  "the EXISTS twin of the two above, in RETURN position so it reaches the evaluator",
			want: "true\x1f",
		},
		{
			name: "WITH in a block body",
			q:    "MATCH (a:P {id: 3}) RETURN COUNT { MATCH (a)-[:K]->(x) WITH x WHERE x.id > 4 RETURN x }",
			why:  "a WITH is a horizon; Neo4j refuses it through tail = None",
			want: "1\x1f",
		},
		{
			name: "RETURN DISTINCT",
			q:    "MATCH (a:P {id: 3}) RETURN COUNT { MATCH (a)-[:K]->(x) RETURN DISTINCT x }",
			why:  "DISTINCT changes the row count; Neo4j refuses it because a DistinctQueryProjection makes CreateIrExpressions append a tail",
			// 2 is correct here, but only COINCIDENTALLY: n3's two :K targets are
			// already distinct, so dropping the DISTINCT — which
			// [ir.TranslateSubquery] does, see the "RETURN that aggregates" case —
			// cannot be observed on this fixture. The value is asserted because it
			// IS the right answer; it is not evidence that DISTINCT is honoured.
			want: "2\x1f",
		},
		{
			name: "RETURN with a LIMIT",
			q:    "MATCH (a:P {id: 3}) RETURN COUNT { MATCH (a)-[:K]->(x) RETURN x LIMIT 1 }",
			why:  "a LIMIT truncates the rows the count sees; Neo4j refuses it through QueryPagination.empty",
			// No value, for the reason given on "RETURN that aggregates": the
			// correct answer is 1 and GoGraph returns 2, because the body's final
			// projection — and this LIMIT with it — is never translated. Refusing
			// the body is what keeps rmp #2648 clear of that defect; the defect
			// itself is out of scope.
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			beforeDeg := degreeRewriteCount.Load()
			beforeHop := labelledHopRewriteCount.Load()
			got := degreeRun(t, on, tc.q)
			if fired := degreeRewriteCount.Load() - beforeDeg; fired != 0 {
				t.Errorf("the DEGREE rewrite fired %d time(s) for a body outside the normalisation's "+
					"boundary — the guard has widened.\n  query: %s\n  why it must not fire: %s", fired, tc.q, tc.why)
			}
			if fired := labelledHopRewriteCount.Load() - beforeHop; fired != 0 {
				t.Errorf("the LABELLED SINGLE-HOP count fired %d time(s) for a body outside the "+
					"normalisation's boundary — the guard has widened.\n  query: %s\n  why it must not fire: %s", fired, tc.q, tc.why)
			}
			if len(got) == 0 {
				t.Fatalf("the query returned no rows, so the answer check below is vacuous: %s", tc.q)
			}
			// Refusing must cost work and not correctness: the same query read by
			// an engine that could not have taken the rewrite anyway must agree.
			unrewritten := oracleRun(t, off, tc.q)
			assertRowsEqual(t, got, unrewritten, "a refused body disagrees with its unrewritten reading", tc.q)
			if tc.want != "" && got[0] != tc.want {
				t.Errorf("absolute value is wrong: got %v, want %q\n  query: %s", got, tc.want, tc.q)
			}
		})
	}
}

// TestBlockFormNormalisation_OptionalMatchIsNotADegree is the sharpened form of
// the OPTIONAL MATCH case above, and the reason that case cannot be softened
// into "conservatism".
//
// n0 has out-degree 0. `MATCH (a)-[:K]->(:Q)` therefore has no match, and
// `OPTIONAL MATCH (a)-[:K]->(:Q)` emits one row of nulls instead of none. If the
// normalisation ever admitted an [ast.OptionalMatch] — which it refuses by type
// assertion, not by an enumerated clause — the two would collapse onto one
// answer. This asserts they do not, WITHOUT pinning which of the two values
// GoGraph produces for the optional form, which is not this task's subject.
func TestBlockFormNormalisation_OptionalMatchIsNotADegree(t *testing.T) {
	g := degreeFixture(t, 60)
	on, _ := adjacencyCountEngines(g)

	plain := degreeRun(t, on, "MATCH (a:P {id: 0}) RETURN COUNT { MATCH (a)-[:K]->(:Q) }")
	optional := degreeRun(t, on, "MATCH (a:P {id: 0}) RETURN COUNT { OPTIONAL MATCH (a)-[:K]->(:Q) }")

	if len(plain) == 0 || len(optional) == 0 {
		t.Fatalf("both forms must return a row; got plain=%v optional=%v", plain, optional)
	}
	if plain[0] != "0\x1f" {
		t.Fatalf("the plain form over a zero-degree anchor returned %v, want 0 — the fixture "+
			"assumption is broken and the comparison below proves nothing", plain)
	}
	if optional[0] == plain[0] {
		t.Errorf("OPTIONAL MATCH and MATCH returned the same count (%v) over a zero-degree "+
			"anchor. Either the optional row of nulls is not being produced, or the "+
			"normalisation has started treating the two as the same body — and if it is the "+
			"latter, the count is now wrong rather than slow.", optional)
	}
}

// TestPatternFormOf_IsInverseOfDesugaring is obligation 3, and it is what makes
// obligation 1 sound.
//
// [countToSingleQuery] and [existsToSingleQuery] turn a pattern form into a
// synthetic single-MATCH body for the translator. [ir.PatternFormOf] turns a
// single-MATCH body back into a pattern form for the recognisers. If the second
// is the exact inverse of the first over every pattern form, then normalising a
// block form that HAS that shape cannot change the query's meaning — the two
// spellings are the same AST modulo one wrapper.
//
// Pointer identity is asserted, not structural equality: a normalisation that
// rebuilt an equivalent-looking pattern would be a second place for the AST's
// meaning to be decided, and this test exists to rule that out.
func TestPatternFormOf_IsInverseOfDesugaring(t *testing.T) {
	for _, src := range []string{
		"MATCH (a) RETURN COUNT { (a)-[:K]->(:Q) }",
		"MATCH (a) RETURN COUNT { (a)-[:K]->(b) }",
		"MATCH (a) RETURN COUNT { (a)-->() }",
		"MATCH (a) RETURN COUNT { (a)-[:K]->(b) WHERE b:Q }",
		"MATCH (a) RETURN COUNT { (a)-[:K]->(b) WHERE b.id > 4 }",
		"MATCH (a) RETURN EXISTS { (a)-[:K]->(:Q) }",
		"MATCH (a) RETURN EXISTS { (a)-[:K]->(b) WHERE b:Q }",
	} {
		t.Run(src, func(t *testing.T) {
			sub := parseSubqueryExpr(t, src)

			var (
				body      *ast.SingleQuery
				wantPat   *ast.Pattern
				wantWhere *ast.Where
			)
			switch s := sub.(type) {
			case *ast.CountSubquery:
				body, wantPat, wantWhere = countToSingleQuery(s), s.Pattern, s.Where
			case *ast.ExistsSubquery:
				body, wantPat, wantWhere = existsToSingleQuery(s), s.Pattern, s.Where
			default:
				t.Fatalf("parsed expression is %T, not a subquery", sub)
			}
			if wantPat == nil {
				t.Fatalf("the parser produced no pattern form for %q, so this case tests nothing", src)
			}

			gotPat, gotWhere, ok := ir.PatternFormOf(body)
			if !ok {
				t.Fatalf("PatternFormOf refused the body that the desugaring itself built from a "+
					"pattern form, so the two are not inverses.\n  query: %s", src)
			}
			if gotPat != wantPat {
				t.Errorf("PatternFormOf returned a DIFFERENT pattern pointer than the one the "+
					"desugaring wrapped; the normalisation is rebuilding the AST instead of "+
					"unwrapping it.\n  query: %s", src)
			}
			if gotWhere != wantWhere {
				t.Errorf("PatternFormOf returned where=%v, want the wrapped clause %v — a dropped "+
					"predicate here is rmp #2242 one layer up.\n  query: %s", gotWhere, wantWhere, src)
			}
		})
	}
}

// TestPatternFormOf_RefusesEveryOtherBody pins the boundary at the unit level,
// without an engine. The query-level suite above proves the recognisers do not
// fire; this proves WHY, at the one function that decides it, so a future change
// that widens the boundary fails here first and with a clearer message.
func TestPatternFormOf_RefusesEveryOtherBody(t *testing.T) {
	for _, tc := range []struct{ name, src, why string }{
		{"RETURN", "MATCH (a) RETURN COUNT { MATCH (a)-[:K]->(b) RETURN b }", "a horizon"},
		{"RETURN DISTINCT", "MATCH (a) RETURN COUNT { MATCH (a)-[:K]->(b) RETURN DISTINCT b }", "a horizon that dedupes"},
		{"RETURN with LIMIT", "MATCH (a) RETURN COUNT { MATCH (a)-[:K]->(b) RETURN b LIMIT 1 }", "a horizon that truncates"},
		{"WITH", "MATCH (a) RETURN COUNT { MATCH (a)-[:K]->(b) WITH b RETURN b }", "a horizon"},
		{"OPTIONAL MATCH", "MATCH (a) RETURN COUNT { OPTIONAL MATCH (a)-[:K]->(b) }", "left-outer semantics, not a match count"},
		{"two MATCH clauses", "MATCH (a) RETURN COUNT { MATCH (a)-[:K]->(b) MATCH (b)-[:K]->(c) }", "a join"},
		{"UNWIND then MATCH", "MATCH (a) RETURN COUNT { UNWIND [1] AS u MATCH (a)-[:K]->(b) }", "two reading clauses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := parseSubqueryExpr(t, tc.src)
			body, ok := sub.(*ast.CountSubquery)
			if !ok {
				t.Fatalf("parsed expression is %T, not a COUNT subquery", sub)
			}
			if body.Query == nil {
				t.Fatalf("the parser did not produce a block form for %q, so this case tests "+
					"nothing about the boundary", tc.src)
			}
			if _, _, admitted := ir.PatternFormOf(body.Query); admitted {
				t.Errorf("PatternFormOf ADMITTED a body carrying %s. The pattern it hands back "+
					"would be read as if the rest of the body were not there.\n  query: %s", tc.why, tc.src)
			}
		})
	}
}

// TestPatternFormOf_AdmitsTheTargetShape is the non-vacuity half of the test
// above: without it, a PatternFormOf that returned false unconditionally would
// pass every case there.
func TestPatternFormOf_AdmitsTheTargetShape(t *testing.T) {
	for _, src := range []string{
		"MATCH (a) RETURN COUNT { MATCH (a)-[:K]->(:Q) }",
		"MATCH (a) RETURN COUNT { MATCH (a)-[:K]->(b) WHERE b:Q }",
		"MATCH (a) RETURN COUNT { MATCH (a)-->() }",
	} {
		t.Run(src, func(t *testing.T) {
			sub := parseSubqueryExpr(t, src)
			body, ok := sub.(*ast.CountSubquery)
			if !ok || body.Query == nil {
				t.Fatalf("the parser did not produce a block-form COUNT for %q (got %T)", src, sub)
			}
			pat, _, admitted := ir.PatternFormOf(body.Query)
			if !admitted {
				t.Fatalf("PatternFormOf refused the target shape; the whole normalisation is "+
					"inert.\n  query: %s", src)
			}
			if pat == nil {
				t.Fatalf("PatternFormOf admitted %q but returned a nil pattern, which every "+
					"recogniser rejects on its first line — the same inertness by another route", src)
			}
		})
	}
}

// parseSubqueryExpr parses src and returns the expression of its first RETURN
// item, which every case in this file writes as the subquery under test.
func parseSubqueryExpr(t *testing.T, src string) ast.Expression {
	t.Helper()
	q, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	sq, ok := q.(*ast.SingleQuery)
	if !ok {
		t.Fatalf("Parse(%q) returned %T, want *ast.SingleQuery", src, q)
	}
	if sq.Return == nil || sq.Return.Projection == nil || len(sq.Return.Projection.Items) == 0 {
		t.Fatalf("Parse(%q) produced no RETURN item to read the subquery from", src)
	}
	return sq.Return.Projection.Items[0].Expr
}

// assertRowsEqual compares two renderings row by row, failing with the context
// the caller supplies. The rows come from [degreeRun], which flattens each one
// to a single comparable string.
func assertRowsEqual(t *testing.T, got, want []string, what, query string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: row count %d vs %d\n  query: %s", what, len(got), len(want), query)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s at row %d:\n  got  %q\n  want %q\n  query: %s", what, i, got[i], want[i], query)
		}
	}
}
