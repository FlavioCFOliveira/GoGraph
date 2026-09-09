package cypher

// exists_where_body_projection_test.go — rmp #2779: a WHERE-position
// `EXISTS { … }` and the same predicate with `AND true` appended must answer
// IDENTICALLY, and both must answer over the body's own final projection.
//
// # What was wrong, in two halves
//
// A WHERE predicate that IS a top-level EXISTS is not evaluated as an
// expression: cypher/ir/exists.go lowers it to a [ir.SemiApply] (or an
// [ir.AntiSemiApply] for NOT EXISTS) whose Inner is the body's plan. Appending
// `AND true` makes the predicate a BinaryOp, which is no longer a top-level
// EXISTS, so it goes down the expression path through [ir.TranslateSubquery]
// instead. rmp #2675 fixed the trailing-RETURN loss on that path only, so from
// then until this fix the two spellings of one predicate disagreed.
//
// HALF 1 — the translator. existsSubPlan looped the body's ReadingClauses and
// returned, never reading SingleQuery.Return, so the body's DISTINCT, ORDER BY,
// SKIP, LIMIT and any aggregation were dropped. EXISTS is "the body produced at
// least one row", so this was wrong in both directions: a body truncated to
// nothing read TRUE, and an aggregating body over an empty match read FALSE.
//
// HALF 2 — the physical builder. Half 1 ALONE is a regression, and rmp #2675
// measured it: applying that one-line change took the openCypher TCK from 3897
// to 3892 — ExistentialSubquery2 [1] [2] and ExistentialSubquery3 [1] [2] [3] —
// with `RETURN n` yielding [null]. cypher/api.go threads ONE column-index map
// through the whole physical build, and buildIRProjection's post-projection
// reset deletes every key it does not keep and rebases the survivors to
// 0..len(items)-1. A Projection on the inner side of a SemiApply therefore wiped
// the OUTER query's variables out of the map, while the operator forwards the
// outer row unchanged — so the outer variables resolved to absent slots and read
// null. cypher/api.go's SemiApply and AntiSemiApply cases now snapshot the outer
// schema and restore it verbatim after the inner build, the same remedy
// [ir.RollUpApply] already applied for the same reason.
//
// Half 2 also fixes a SECOND, independent manifestation that needed no RETURN in
// the body at all: every inner scan and Expand appends columns at
// schemaWidth(schema), and that inflated width survived the inner build, so a
// clause built ABOVE the SemiApply mis-offset its own bindings against the
// narrower row it actually receives. See
// TestExistsWhereSchemaIsolation_FollowingClauseBindsCorrectly.
//
// # Where the required answers come from
//
// NOT from the openCypher TCK for the row-count rules: the TCK's thirteen
// brace-subquery occurrences are all WHERE-position `exists { }` and none of them
// carries a LIMIT, a SKIP or an aggregation in its body. The TCK is what pins
// HALF 2 — it is the gate that refuted the naive fix — and the authority for the
// row-count semantics is the reference implementation plus this repo's own TCK
// for the sub-rules it does cover. The full citation chain is in the header of
// cypher/count_subquery_body_projection_test.go; in short:
//
//   - github.com/neo4j/neo4j, release tag 2026.07.1 (commit
//     f213380f812b820a1b312e2ea52cb3d8f1931ccc), CreateIrExpressions.scala,
//     `case existsExpression @ ExistsExpression(q)`: the body is converted WHOLE
//     with no horizon override, so EXISTS is "the body produced at least one
//     row", evaluated over the body's own final projection;
//   - cypher/tck/features/clauses/return/Return4.feature scenario [6]: an
//     aggregation with no grouping key emits exactly one row even over an empty
//     input — which is why an aggregating body over a non-matching anchor makes
//     EXISTS TRUE and NOT EXISTS FALSE.
//
// ORACLE — from those, the answer is decided by the number of rows the body
// produces. Every case below therefore carries its body spelled as an ORDINARY
// query after the same anchor, and the suite measures that spelling's row count
// and requires it to equal the hand-computed wantBodyRows before deriving the
// expected EXISTS answer from it. The top-level reading shares no code with
// either subquery path and is governed by the TCK, so it is an independent
// oracle rather than a restatement.
//
// # Every case is evidence, and the table says for which half
//
// headWrong and naiveWrong were MEASURED, by building the tree in each of those
// two states and running this exact case list. A case for which both are false
// would prove nothing about this defect, and
// TestExistsWhereBodyProjection_EveryCaseIsEvidence rejects the table outright
// if one appears. Measured: headWrong in 4 of the 11 cases, naiveWrong in 10.
//
// The counters [degreeRewriteCount] and [labelledHopRewriteCount] are
// process-global, so no test in this file may call t.Parallel.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	lpg "github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// nullRow is one row of one column rendered as NULL by [degreeRun]. It is the
// signature of the refuted naive fix: the outer variable resolving to a slot the
// forwarded outer row does not carry.
const nullRow = "null\x1f"

// existsWhereCase is one subquery body, read four ways: through the
// WHERE-position SemiApply lowering, through the expression path that `AND true`
// forces, as the ordinary top-level query the body is sugar for, and as a
// hand-computed row count.
type existsWhereCase struct {
	name string
	// anchor is the outer MATCH, and anchorVar the variable it binds. The
	// projected column is always <anchorVar>.id, so a lost outer binding shows
	// up as NULL rather than as a missing row.
	anchor, anchorVar string
	// body is the subquery body, without the enclosing braces.
	body string
	// wantBodyRows is how many rows `anchor body` must produce as an ordinary
	// query. The suite asserts it, so a wrong oracle fails loudly instead of
	// agreeing with a wrong answer.
	wantBodyRows int
	// headWrong records that at least ONE of this case's four arms answered
	// differently BEFORE this fix than it must — so the case is evidence for
	// HALF 1. Both lowering arms are covered, because the defect inverted EXISTS
	// and NOT EXISTS in opposite directions and a case can discriminate on
	// either; the arm is named in why.
	headWrong bool
	// naiveWrong records the same measurement with HALF 1 applied ALONE, i.e.
	// the exact change rmp #2675 measured at TCK 3892 — so the case is evidence
	// for HALF 2, the schema isolation.
	naiveWrong bool
	why        string
}

func existsWhereCases() []existsWhereCase {
	return []existsWhereCase{
		{
			name: "aggregating RETURN over an EMPTY match", anchor: "MATCH (z:Anchor {id: 9})", anchorVar: "z",
			body: "MATCH (z)-[:K]->(x) RETURN count(*)", wantBodyRows: 1,
			headWrong: true, naiveWrong: true,
			why: "z has no outgoing :K, yet count(*) with no grouping key still emits one row " +
				"(TCK Return4.feature [6]), so EXISTS is TRUE and NOT EXISTS is FALSE. Before the " +
				"fix both lowering arms were inverted; with the translator half alone the EXISTS " +
				"arm kept its row but z.id read NULL",
		},
		{
			name: "RETURN with LIMIT 0", anchor: "MATCH (a:Anchor {id: 0})", anchorVar: "a",
			body: "MATCH (a)-[:K]->(x) RETURN x LIMIT 0", wantBodyRows: 0,
			headWrong: true, naiveWrong: true,
			why: "LIMIT 0 truncates the body to no rows, so the existence test must FAIL even " +
				"though the pattern matches twice. This is the shape the task singles out: a " +
				"dropped LIMIT is invisible to an existence test unless the LIMIT can empty the " +
				"body. With the translator half alone the EXISTS arm became correct while the NOT " +
				"EXISTS arm kept the row and read a.id as NULL — which is why the flag is set",
		},
		{
			name: "RETURN with a SKIP past the end", anchor: "MATCH (a:Anchor {id: 0})", anchorVar: "a",
			body: "MATCH (a)-[:K]->(x) RETURN x SKIP 2", wantBodyRows: 0,
			headWrong: true, naiveWrong: true,
			why: "SKIP 2 consumes both matches, so the body is empty and the existence test fails. " +
				"As with LIMIT 0, the surviving NOT EXISTS row read a.id as NULL under the " +
				"translator half alone",
		},
		{
			name: "WITH, then a RETURN with LIMIT 0", anchor: "MATCH (a:Anchor {id: 0})", anchorVar: "a",
			body: "MATCH (a)-[:K]->(x) WITH x ORDER BY x.ord RETURN x LIMIT 0", wantBodyRows: 0,
			headWrong: true, naiveWrong: true,
			why: "the WITH reaches the plan through ReadingClauses and was never dropped, so BOTH " +
				"halves are exercised here: before the fix the EXISTS arm read a.id as NULL — the " +
				"WITH's own Projection already wiped the shared schema, with no help from HALF 1 — " +
				"AND kept a row it must not keep",
		},
		{
			name: "RETURN with a SKIP inside the body", anchor: "MATCH (a:Anchor {id: 0})", anchorVar: "a",
			body: "MATCH (a)-[:K]->(x) RETURN x SKIP 1", wantBodyRows: 1,
			headWrong: false, naiveWrong: true,
			why: "a SKIP that does not exhaust the body leaves it non-empty, so the existence " +
				"ANSWER cannot discriminate — the case is here for HALF 2, where the inner " +
				"Projection nulled a.id on the EXISTS arm",
		},
		{
			name: "RETURN with ORDER BY and LIMIT", anchor: "MATCH (a:Anchor {id: 0})", anchorVar: "a",
			body: "MATCH (a)-[:K]->(x) RETURN x ORDER BY x.ord LIMIT 1", wantBodyRows: 1,
			headWrong: false, naiveWrong: true,
			why: "the ordered-and-truncated form, which GoGraph fuses into a Top; evidence for HALF 2",
		},
		{
			name: "RETURN DISTINCT over a genuine duplicate", anchor: "MATCH (a:Anchor {id: 0})", anchorVar: "a",
			body: "MATCH (a)-[:K]->(x) RETURN DISTINCT x.v", wantBodyRows: 1,
			headWrong: false, naiveWrong: true,
			why: "t1.v = t2.v = 1, so DISTINCT really removes a row here — but one row remains, so " +
				"the existence answer is unchanged and the evidence is for HALF 2",
		},
		{
			name: "aggregating RETURN with a grouping key", anchor: "MATCH (a:Anchor {id: 0})", anchorVar: "a",
			body: "MATCH (a)-[:K]->(x) RETURN x.v, count(*)", wantBodyRows: 1,
			headWrong: false, naiveWrong: true,
			why: "both targets share v, so the two matches group into ONE row; evidence for HALF 2",
		},
		{
			name: "plain RETURN", anchor: "MATCH (a:Anchor {id: 0})", anchorVar: "a",
			body: "MATCH (a)-[:K]->(x) RETURN x", wantBodyRows: 2,
			headWrong: false, naiveWrong: true,
			why: "the shape the openCypher TCK's own RETURN-bearing EXISTS bodies have " +
				"(ExistentialSubquery2 [1]). Cardinality-preserving by specification, so it is pure " +
				"HALF 2 evidence — and it is the exact case that took the TCK from 3897 to 3892",
		},
		{
			name:   "body projects the outer property the outer query also reads",
			anchor: "MATCH (a:Anchor {id: 0})", anchorVar: "a",
			body: "MATCH (a)-[:K]->(x) RETURN a.id", wantBodyRows: 2,
			headWrong: false, naiveWrong: true,
			why: "the inner projection registers the SECONDARY expression key \"a.id\" at its own " +
				"index 0, which the outer projection's fast path then reads out of the outer row — " +
				"so with the translator half alone this returned the NODE bound to a, rendered as " +
				"its handle, instead of the integer 0. A name collision, not an absent slot",
		},
		{
			name:   "body aliases a column with the outer variable's own name",
			anchor: "MATCH (a:Anchor {id: 0})", anchorVar: "a",
			body: "MATCH (a)-[:K]->(x) RETURN 1 AS a", wantBodyRows: 2,
			headWrong: false, naiveWrong: false,
			why: "the one case in the table that discriminates NEITHER half, kept deliberately: the " +
				"inner alias `a` lands at index 0, which COINCIDES with the outer index of `a`, so " +
				"the corruption is invisible. It is here as the control that the coincidence is " +
				"understood rather than relied upon — see EveryCaseIsEvidence, which exempts it by name",
		},
	}
}

// nonEvidenceByDesign names the single case above that discriminates neither
// half. Naming it here — rather than deriving it from the flags — is what makes
// TestExistsWhereBodyProjection_EveryCaseIsEvidence able to reject a case that
// decays into non-evidence by accident.
var nonEvidenceByDesign = map[string]string{
	"body aliases a column with the outer variable's own name": "inner alias index coincides with the outer one",
}

// TestExistsWhereBodyProjection_EveryCaseIsEvidence rejects the table itself if
// a case proves nothing about either half of the defect, unless it is named in
// [nonEvidenceByDesign] with a reason. It also asserts that each half really is
// discriminated by at least one case, so the suite cannot silently become a
// one-sided gate.
func TestExistsWhereBodyProjection_EveryCaseIsEvidence(t *testing.T) {
	var head, naive int
	for _, tc := range existsWhereCases() {
		reason, exempt := nonEvidenceByDesign[tc.name]
		switch {
		case tc.headWrong || tc.naiveWrong:
			if exempt {
				t.Errorf("case %q is exempted as non-evidence (%q) but its flags say it "+
					"discriminates; remove it from nonEvidenceByDesign", tc.name, reason)
			}
		case !exempt:
			t.Errorf("case %q discriminates NEITHER half of rmp #2779 and is not named in "+
				"nonEvidenceByDesign: it cannot fail on the defective code, so it is not evidence",
				tc.name)
		}
		if tc.headWrong {
			head++
		}
		if tc.naiveWrong {
			naive++
		}
	}
	if head == 0 {
		t.Error("no case discriminates HALF 1 (the dropped trailing RETURN); the suite would pass " +
			"with existsSubPlan's q.Return branch removed")
	}
	if naive == 0 {
		t.Error("no case discriminates HALF 2 (schema isolation); the suite would pass with the " +
			"SemiApply schema restore removed, which is the change rmp #2675 measured at TCK 3892")
	}
	t.Logf("table discriminates HALF 1 in %d case(s) and HALF 2 in %d case(s), of %d",
		head, naive, len(existsWhereCases()))
}

// TestExistsWhereBodyProjection_OracleHolds pins every row count the expected
// answers are derived from, BEFORE any case asserts an answer. The body spelled
// as an ordinary query is the independent oracle; if it does not produce the
// hand-computed number then the fixture, not the engine, is what changed.
func TestExistsWhereBodyProjection_OracleHolds(t *testing.T) {
	eng := NewEngine(bodyProjectionFixture(t))
	for _, tc := range existsWhereCases() {
		t.Run(tc.name, func(t *testing.T) {
			q := tc.anchor + " " + tc.body
			got := degreeRun(t, eng, q)
			if len(got) != tc.wantBodyRows {
				t.Fatalf("the body spelled as an ordinary query produced %d row(s), want %d.\n"+
					"  query: %s\n  rows: %q\n"+
					"  Every expected EXISTS answer in this file is derived from this count, so the "+
					"table is now unsound rather than merely failing.", len(got), tc.wantBodyRows, q, got)
			}
			for i, row := range got {
				if row == nullRow {
					t.Fatalf("the oracle query returned NULL in row %d, so it cannot decide "+
						"anything.\n  query: %s", i, q)
				}
			}
		})
	}
}

// TestExistsWhereBodyProjection_SpellingsAgree is the acceptance criterion of
// rmp #2779: the WHERE-position spelling and the `AND true` spelling must return
// the SAME rows, and both must return the rows the body's own row count
// requires.
//
// Four arms per case, because the defect had two independently wrong directions
// and NOT EXISTS inverts both:
//
//	EXISTS { … }              → keep the anchor row iff the body produced a row
//	EXISTS { … } AND true     → the expression path, correct since rmp #2675
//	NOT EXISTS { … }          → keep it iff the body produced NO row
//	NOT EXISTS { … } AND true → the expression path again
//
// The projected column is <anchorVar>.id rather than the anchor itself, so a
// lost outer binding surfaces as a NULL value and not merely as a missing row —
// the two failure modes of this defect are different and must not be conflated.
func TestExistsWhereBodyProjection_SpellingsAgree(t *testing.T) {
	eng := NewEngine(bodyProjectionFixture(t))
	for _, tc := range existsWhereCases() {
		t.Run(tc.name, func(t *testing.T) {
			// Derived from the oracle asserted by OracleHolds, never restated.
			kept := degreeRun(t, eng, tc.anchor+" RETURN "+tc.anchorVar+".id")
			if len(kept) != 1 {
				t.Fatalf("the anchor %q matched %d rows, want exactly 1; the arms below could not "+
					"distinguish a filtered row from a missing one", tc.anchor, len(kept))
			}
			wantExists := []string(nil)
			if tc.wantBodyRows > 0 {
				wantExists = kept
			}
			wantNotExists := kept
			if tc.wantBodyRows > 0 {
				wantNotExists = nil
			}

			arms := []struct {
				label, query string
				want         []string
			}{
				{
					"EXISTS (SemiApply lowering)",
					tc.anchor + " WHERE EXISTS { " + tc.body + " } RETURN " + tc.anchorVar + ".id",
					wantExists,
				},
				{
					"EXISTS AND true (expression path)",
					tc.anchor + " WHERE EXISTS { " + tc.body + " } AND true RETURN " + tc.anchorVar + ".id",
					wantExists,
				},
				{
					"NOT EXISTS (AntiSemiApply lowering)",
					tc.anchor + " WHERE NOT EXISTS { " + tc.body + " } RETURN " + tc.anchorVar + ".id",
					wantNotExists,
				},
				{
					"NOT EXISTS AND true (expression path)",
					tc.anchor + " WHERE NOT EXISTS { " + tc.body + " } AND true RETURN " + tc.anchorVar + ".id",
					wantNotExists,
				},
			}

			results := make([][]string, len(arms))
			for i, arm := range arms {
				results[i] = degreeRun(t, eng, arm.query)
				if !sameRows(results[i], arm.want) {
					t.Errorf("%s answered %q, want %q.\n  query: %s\n  body rows: %d\n  why: %s",
						arm.label, results[i], arm.want, arm.query, tc.wantBodyRows, tc.why)
				}
			}
			// The disagreement itself, asserted directly rather than inferred from
			// the two answers matching `want`. This is the observable the task
			// names, and it must be reported as such when it reappears.
			if !sameRows(results[0], results[1]) {
				t.Errorf("the two spellings of one predicate DISAGREE (rmp #2779): "+
					"EXISTS { … } answered %q and EXISTS { … } AND true answered %q.\n"+
					"  body: %s", results[0], results[1], tc.body)
			}
			if !sameRows(results[2], results[3]) {
				t.Errorf("the two spellings of one predicate DISAGREE (rmp #2779): "+
					"NOT EXISTS { … } answered %q and NOT EXISTS { … } AND true answered %q.\n"+
					"  body: %s", results[2], results[3], tc.body)
			}
		})
	}
}

// TestExistsWhereSchemaIsolation_FollowingClauseBindsCorrectly is the SECOND
// manifestation of the missing schema isolation, and it needs no RETURN in the
// body at all — so it fails on the tree as it stood before this fix, with HALF 1
// not yet applied.
//
// Every inner scan and Expand registers its columns at schemaWidth(schema), and
// that inflated width survived the inner build of a SemiApply. A clause built
// ABOVE the operator then allocated its own fresh columns past the inflated
// width, while [exec.SemiApply] forwards the OUTER row — narrower — so those
// bindings addressed slots the row does not have. `y.ord` read NULL on every
// row while the identical query WITHOUT the EXISTS answered correctly, which is
// what identifies the SemiApply as the cause rather than the second MATCH.
func TestExistsWhereSchemaIsolation_FollowingClauseBindsCorrectly(t *testing.T) {
	eng := NewEngine(bodyProjectionFixture(t))
	const tail = " MATCH (a)-[:K]->(y) RETURN a.id, y.ord"
	want := degreeRun(t, eng, "MATCH (a:Anchor {id: 0})"+tail)

	// The control must itself be sound: two rows, both fully bound. Without this
	// the comparison could pass by both arms being equally broken.
	if len(want) != 2 {
		t.Fatalf("the no-EXISTS control produced %d row(s), want 2: %q", len(want), want)
	}
	for i, row := range want {
		if row == nullRow || row == "0\x1f"+nullRow {
			t.Fatalf("the no-EXISTS control is already NULL in row %d (%q), so it cannot serve as "+
				"the oracle", i, row)
		}
	}

	for _, prefix := range []string{
		// A body with NO projection of any kind: pure width inflation.
		"MATCH (a:Anchor {id: 0}) WHERE EXISTS { MATCH (a)-[:K]->(x) }",
		"MATCH (a:Anchor {id: 0}) WHERE NOT EXISTS { MATCH (a)-[:M]->(x) }",
		// A body WITH a projection: inflation and destruction together.
		"MATCH (a:Anchor {id: 0}) WHERE EXISTS { MATCH (a)-[:K]->(x) RETURN x }",
		"MATCH (a:Anchor {id: 0}) WHERE NOT EXISTS { MATCH (a)-[:M]->(x) RETURN x }",
	} {
		t.Run(prefix, func(t *testing.T) {
			got := degreeRun(t, eng, prefix+tail)
			if !sameRows(got, want) {
				t.Errorf("a clause after the EXISTS mis-bound its columns (rmp #2779): got %q, "+
					"want %q — the same rows the identical query without the EXISTS returns.\n"+
					"  query: %s", got, want, prefix+tail)
			}
		})
	}
}

// TestExistsWhereBodyProjection_TCKShapesPreserved is the local guard against
// the refutation rmp #2675 recorded.
//
// These five queries are the openCypher TCK scenarios that fell when the
// translator half was applied alone: ExistentialSubquery2 [1] and [2], and
// ExistentialSubquery3 [1], [2] and [3]. Each has a RETURN-bearing exists body
// in WHERE position and projects an OUTER variable, which is precisely the
// combination that read [null]. The TCK remains the authority and the gate — it
// runs the real scenarios with the real comparison semantics — but a 3897-
// scenario suite is not what a future change will run first, and a bare `RETURN
// n` yielding null deserves a failure that names this defect.
//
// The graphs and queries are transcribed from
// cypher/tck/features/expressions/existentialSubqueries/, so the assertions here
// are deliberately weaker than the TCK's: exactly one row, the projected node
// NOT null, and it being the :A node.
func TestExistsWhereBodyProjection_TCKShapesPreserved(t *testing.T) {
	const fixtureA = `CREATE (a:A {prop: 1})-[:R]->(b:B {prop: 1}), (a)-[:R]->(:C {prop: 2}), (a)-[:R]->(:D {prop: 3})`
	const fixtureB = `CREATE (a:A {prop: 1})-[:R]->(b:B {prop: 1}), (a)-[:R]->(:C {prop: 2}), (a)-[:R]->(d:D {prop: 3}), (b)-[:R]->(d)`

	cases := []struct{ name, setup, where string }{
		{
			"ExistentialSubquery2 [1] Full existential subquery",
			fixtureA,
			"exists { MATCH (n)-->() RETURN true }",
		},
		{
			"ExistentialSubquery2 [2] Full existential subquery with aggregation",
			fixtureB,
			"exists { MATCH (n)-->(m) WITH n, count(*) AS numConnections WHERE numConnections = 3 RETURN true }",
		},
		{
			"ExistentialSubquery3 [1] Nested simple existential subquery",
			fixtureA,
			"exists { MATCH (m) WHERE exists { (n)-[]->(m) WHERE n.prop = m.prop } RETURN true }",
		},
		{
			"ExistentialSubquery3 [2] Nested full existential subquery",
			fixtureA,
			"exists { MATCH (m) WHERE exists { MATCH (l)<-[:R]-(n)-[:R]->(m) RETURN true } RETURN true }",
		},
		{
			"ExistentialSubquery3 [3] Nested full existential subquery with pattern predicate",
			fixtureA,
			"exists { MATCH (m) WHERE exists { MATCH (l) WHERE (l)<-[:R]-(n)-[:R]->(m) RETURN true } RETURN true }",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := tckShapeEngine(t, tc.setup)

			// `RETURN n` is the TCK's own projection and the read that went null.
			nodes := degreeRun(t, eng, "MATCH (n) WHERE "+tc.where+" RETURN n")
			if len(nodes) != 1 {
				t.Fatalf("scenario returned %d row(s), want 1: %q", len(nodes), nodes)
			}
			if nodes[0] == nullRow {
				t.Errorf("the outer variable `n` read NULL (rmp #2779 / rmp #2675's refuted fix): " +
					"the inner Projection re-registered columns in the shared column-index map at " +
					"inner-side indices while SemiApply forwards the narrower outer row. This is " +
					"the exact failure that took the TCK from 3897 to 3892.")
			}
			// And it is the right node, not merely a non-null one.
			props := degreeRun(t, eng, "MATCH (n) WHERE "+tc.where+" RETURN n.prop")
			if len(props) != 1 || props[0] != "1\x1f" {
				t.Errorf("the surviving row is not the :A node: n.prop = %q, want [\"1\"]", props)
			}
		})
	}
}

// tckShapeEngine builds a fresh engine and applies one setup statement,
// mirroring the TCK harness's "Given an empty graph / And having executed".
func tckShapeEngine(t *testing.T, setup string) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	eng := NewEngine(g)
	res, err := eng.RunAny(context.Background(), setup, nil)
	if err != nil {
		t.Fatalf("setup %q: %v", setup, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("setup %q drain: %v", setup, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("setup %q close: %v", setup, err)
	}
	return eng
}

// sameRows compares two row renderings elementwise, treating nil and empty as
// equal. [degreeRun] returns rows in the engine's own order; every query in this
// file either returns at most one row or is compared against the identically
// ordered control, so no sort is applied — an order change is itself a signal.
func sameRows(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
