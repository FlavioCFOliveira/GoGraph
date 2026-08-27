package cypher_test

// exists_write_correlation_test.go — regression battery for rmp #2659:
// a correlated EXISTS { } in the WHERE of a WITH that follows an
// entity-creating write clause silently lost its correlation.
//
// The defect was a silent wrong answer, not an error. Before the fix,
//
//	MATCH (a:P) CREATE (:Q) WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN a.sid AS sid
//
// returned one row per :P node with sid = null, where openCypher requires one
// row per :P node that actually has an outgoing :Z edge to a :P node, carrying
// that node's real sid.
//
// Root cause (cypher/ir/exists.go): the correlation set handed to the inner
// Argument was read from the outer plan's own Vars(), which by contract reports
// only the variables an operator introduces. The top operator of a writing
// pipeline is CreateNode, whose Vars() is just the created node's synthetic
// name, so `a` never reached the Argument. Two failures followed from that one
// omission:
//
//  1. the subquery's `(a)` was planned as a fresh AllNodesScan, so the EXISTS
//     stopped being correlated and reported true for every outer row as soon as
//     any matching relationship existed anywhere in the graph — a wrong row set;
//  2. that AllNodesScan re-registered `a` in the physical builder's shared
//     column-index map at an inner-side index, orphaning the outer row's slot,
//     so every entity-derived projection of `a` evaluated to null.
//
// Both symptoms must be pinned, and pinned separately: a test that checked only
// the sid value would pass on a build that returned the wrong number of rows
// with correct sids, and a test that checked only the row count would pass on a
// build that returned the right rows all-null. Every assertion below therefore
// covers the row set AND the projected value.
//
// The plan-shape counterpart — that the inner subtree is a CorrelatedApply over
// an Argument carrying `a`, and contains no AllNodesScan — is pinned at the IR
// level in cypher/ir/exists_write_correlation_test.go.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// existsWriteBaseSID is the first sid handed to the fixture's :P nodes. The
// values are large and distinct so a null, a zero, or a node id can never be
// mistaken for one of them in a failure message.
const existsWriteBaseSID = 100000

// newExistsWriteFixture builds n :P nodes carrying sid = existsWriteBaseSID+i
// and NO relationships at all. Every :Z edge the variants rely on is created
// either by an explicit setup statement or by the statement under test, so the
// fixture itself can never be the source of a missing property or a stray edge.
func newExistsWriteFixture(t *testing.T, n int) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	e := cypher.NewEngine(g)
	for i := range n {
		q := fmt.Sprintf(`CREATE (:P {sid:%d})`, existsWriteBaseSID+i)
		if _, err := e.RunAny(context.Background(), q, nil); err != nil {
			t.Fatalf("fixture node %d: %v", i, err)
		}
	}
	return e
}

// drainExistsWrite runs q and returns one rendered string per row, sorted so
// the comparison does not depend on scan order, plus the iteration error text.
func drainExistsWrite(t *testing.T, e *cypher.Engine, q string) (rows []string, errText string) {
	t.Helper()
	res, err := e.RunAny(context.Background(), q, nil)
	if err != nil {
		return nil, err.Error()
	}
	for res.Next() {
		rows = append(rows, renderExistsWriteRow(res.Record()))
	}
	if iterErr := res.Err(); iterErr != nil {
		errText = iterErr.Error()
	}
	if closeErr := res.Close(); closeErr != nil && errText == "" {
		errText = closeErr.Error()
	}
	slices.Sort(rows)
	return rows, errText
}

// renderExistsWriteRow formats a record as "k=v" pairs in key order. Only the
// value kinds the matrix projects are spelled out; anything else falls through
// to %v, which is enough to make an unexpected kind visible in a diff.
func renderExistsWriteRow(rec map[string]any) string {
	keys := make([]string, 0, len(rec))
	for k := range rec {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+renderExistsWriteValue(rec[k]))
	}
	return strings.Join(parts, " ")
}

func renderExistsWriteValue(v any) string {
	switch tv := v.(type) {
	case nil:
		return "null"
	case expr.IntegerValue:
		return fmt.Sprintf("%d", int64(tv))
	case expr.BoolValue:
		return fmt.Sprintf("%t", bool(tv))
	case expr.Value:
		if expr.IsNull(tv) {
			return "null"
		}
		return tv.String()
	default:
		return fmt.Sprintf("%v", tv)
	}
}

// sidRows renders the expected rows of a single-column `sid` projection.
func sidRows(sids ...int64) []string {
	out := make([]string, 0, len(sids))
	for _, s := range sids {
		out = append(out, fmt.Sprintf("sid=%d", s))
	}
	slices.Sort(out)
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// The minimal case
// ─────────────────────────────────────────────────────────────────────────────

// TestExistsAfterWriteMinimalCorrelation is the narrowest reproduction of
// #2659. Two :P nodes exist and only sid=1 carries a :Z self-edge, so a
// correlated EXISTS must ship exactly ONE row carrying exactly sid=1.
//
// The two controls are asserted in the same test rather than assumed, because
// the defect's signature is "plausible but wrong": without them, `[{sid:1}]`
// could not be distinguished from a fixture that simply had one node, and a
// null sid could not be distinguished from a sid that was never set.
//
//   - BASELINE  `MATCH (a:P) RETURN a.sid`         → sid is genuinely set on both.
//   - READ-ONLY the same statement without CREATE  → the expected answer, from
//     the same fixture and the same predicate.
//
// Both halves of the defect are pinned: the row COUNT (the filter really is
// correlated) and the sid VALUE (the binding really survived the write). A
// build that regresses either one fails here.
func TestExistsAfterWriteMinimalCorrelation(t *testing.T) {
	e := newExistsWriteFixture(t, 2)
	setup := []string{
		// Only the FIRST node gets a :Z self-edge, so a correlated EXISTS is
		// selective and an uncorrelated one is not. This asymmetry is what makes
		// the row count a real oracle.
		fmt.Sprintf(`MATCH (a:P {sid:%d}) CREATE (a)-[:Z]->(a)`, existsWriteBaseSID),
	}
	for _, s := range setup {
		if _, err := e.RunAny(context.Background(), s, nil); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}

	const predicate = `WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN a.sid AS sid`

	// Control 1 — sid is genuinely set on both nodes. Without this, a null sid
	// in the statement under test would be indistinguishable from an unset one.
	gotBase, errText := drainExistsWrite(t, e, `MATCH (a:P) RETURN a.sid AS sid`)
	if errText != "" {
		t.Fatalf("baseline control: %s", errText)
	}
	wantBase := sidRows(existsWriteBaseSID, existsWriteBaseSID+1)
	if !slices.Equal(gotBase, wantBase) {
		t.Fatalf("baseline control: sid is not set on the fixture\n got: %v\nwant: %v", gotBase, wantBase)
	}

	// Control 2 — the same predicate on the same fixture, WITHOUT the write
	// clause. This establishes the expected answer empirically instead of
	// asserting it from the specification alone.
	gotRead, errText := drainExistsWrite(t, e, `MATCH (a:P) `+predicate)
	if errText != "" {
		t.Fatalf("read-only control: %s", errText)
	}
	wantOne := sidRows(existsWriteBaseSID)
	if !slices.Equal(gotRead, wantOne) {
		t.Fatalf("read-only control: correlated EXISTS is broken even without a write\n got: %v\nwant: %v", gotRead, wantOne)
	}

	// The statement under test: identical to control 2 with an entity-creating
	// write clause inserted. The write touches neither `a` nor the :Z edges the
	// EXISTS reads, so it must not change the answer.
	got, errText := drainExistsWrite(t, e, `MATCH (a:P) CREATE (:Q) `+predicate)
	if errText != "" {
		t.Fatalf("write path: %s", errText)
	}
	// Row COUNT — the filter must still be correlated. Two rows here means the
	// EXISTS became uncorrelated and fired for every outer row.
	if len(got) != 1 {
		t.Errorf("write path: EXISTS lost its correlation: got %d rows, want 1 (#2659 symptom 1)\n got: %v", len(got), got)
	}
	// Row VALUE — the binding of `a` must have survived the write clause. A
	// null here means the outer row's column slot was orphaned.
	if !slices.Equal(got, wantOne) {
		t.Errorf("write path: wrong rows\n got: %v\nwant: %v (#2659)", got, wantOne)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Scope boundaries of the correlation set
// ─────────────────────────────────────────────────────────────────────────────

// TestExistsCorrelationScopeBoundaries guards the OTHER side of #2659: the
// correlation set must not be too WIDE either. Nothing guarded this window
// before, which is how a correlation set built from collectAllVars — which
// descends SemiApply.Inner, against that operator's documented contract that
// "only outer variables are visible downstream" — looked green.
//
// The governing rule is openCypher CIP2015-05-13-EXISTS: both forms of
// existential subquery "are allowed to introduce new variables", and "any
// variables introduced in an <ExistentialSubquery> are not available outside
// the subquery context". So a name that is not in the outer scope at the
// subquery is a BINDING occurrence of a fresh variable, and the subquery is
// legally UNCORRELATED on it — it must not be resolved against the outer row.
//
// Each expectation below is derived from a control in this same test, never
// from "what the code used to do".
func TestExistsCorrelationScopeBoundaries(t *testing.T) {
	// Two :P nodes; only the first carries a :Z self-edge. Any predicate that
	// is genuinely correlated on the outer node is therefore SELECTIVE (1 row),
	// and any predicate that is uncorrelated is not (2 rows). That asymmetry is
	// what makes every row count below a real oracle.
	newFixture := func(t *testing.T) *cypher.Engine {
		t.Helper()
		e := newExistsWriteFixture(t, 2)
		q := fmt.Sprintf(`MATCH (a:P {sid:%d}) CREATE (a)-[:Z]->(a)`, existsWriteBaseSID)
		if _, err := e.RunAny(context.Background(), q, nil); err != nil {
			t.Fatalf("edge setup: %v", err)
		}
		return e
	}

	run := func(t *testing.T, q string) []string {
		t.Helper()
		rows, errText := drainExistsWrite(t, newFixture(t), q)
		if errText != "" {
			t.Fatalf("query %q: %s", q, errText)
		}
		return rows
	}

	// ── Control A: an unambiguously fresh variable ────────────────────────────
	// `zz` appears nowhere outside the subquery, so it cannot be anything but a
	// new variable. This establishes that the UNCORRELATED path exists, is
	// reachable, and answers "true for every outer row" — without it, every
	// uncorrelated expectation below would be an assumption.
	uncorrelated := run(t, `MATCH (a:P) WITH a WHERE EXISTS { MATCH (zz)-[:Z]->(:P) } RETURN a.sid AS sid`)
	wantBoth := sidRows(existsWriteBaseSID, existsWriteBaseSID+1)
	if !slices.Equal(uncorrelated, wantBoth) {
		t.Fatalf("control A: an EXISTS on a demonstrably fresh variable is not uncorrelated\n got: %v\nwant: %v",
			uncorrelated, wantBoth)
	}

	// ── Control B: a genuinely correlated predicate is selective ─────────────
	correlated := run(t, `MATCH (a:P) WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN a.sid AS sid`)
	wantOne := sidRows(existsWriteBaseSID)
	if !slices.Equal(correlated, wantOne) {
		t.Fatalf("control B: a correlated EXISTS is not selective on this fixture\n got: %v\nwant: %v",
			correlated, wantOne)
	}

	// ── Control C: the WITH's own scope cut does not apply to its own WHERE ───
	// openCypher pins this in our own TCK corpus at
	// clauses/with-where/WithWhere7.feature [1] ("WHERE sees a variable bound
	// before but not after WITH"). `a` is not projected by `WITH 1 AS x`, yet
	// the WHERE attached to that WITH still sees it — so the EXISTS here is
	// CORRELATED and must be selective. This control is what makes the S3
	// expectation below a derivation rather than a guess.
	scopeCutControl := run(t, `MATCH (a:P) WITH 1 AS x WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN x`)
	if len(scopeCutControl) != 1 {
		t.Fatalf("control C: WITH's own scope cut wrongly applied to its own WHERE (WithWhere7 [1]): got %d rows, want 1\n got: %v",
			len(scopeCutControl), scopeCutControl)
	}

	t.Run("S1_var_dropped_by_WITH_is_still_in_scope_for_its_WHERE", func(t *testing.T) {
		// Same query as control C. Pinned as its own subtest because it is the
		// probe the earlier collectAllVars attempt was defended with.
		got := run(t, `MATCH (a:P) WITH 1 AS x WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN x`)
		if len(got) != 1 {
			t.Errorf("expected 1 row: `a` is bound before the WITH, so its WHERE sees it (WithWhere7 [1]) and the EXISTS is correlated\n got: %v", got)
		}
	})

	t.Run("S2_prior_subquery_var_is_a_FRESH_variable", func(t *testing.T) {
		// `b` is bound only inside the FIRST subquery. Per CIP2015-05-13-EXISTS
		// it is not available outside that subquery, so the second subquery's
		// `b` is a new variable and that subquery is UNCORRELATED — it is true
		// because some :Z edge to a :P node exists somewhere.
		//
		// The row count therefore comes entirely from the FIRST (correlated)
		// EXISTS: 1 row, matching control B. A correlation set that wrongly
		// claimed `b` resolves it against an absent slot and returns 0 rows.
		got := run(t, `MATCH (a:P) WHERE EXISTS { MATCH (a)-[r:Z]->(b:P) } `+
			`WITH a WHERE EXISTS { MATCH (b)-[:Z]->(:P) } RETURN a.sid AS sid`)
		if !slices.Equal(got, wantOne) {
			t.Errorf("the second subquery's `b` must be a FRESH variable (CIP: subquery variables are not available outside it), "+
				"so the second EXISTS is uncorrelated and only the first EXISTS filters\n got: %v\nwant: %v", got, wantOne)
		}
	})

	t.Run("S4_FOREACH_loop_var_is_local_to_the_body", func(t *testing.T) {
		// FOREACH is an updating clause, so it can precede a WITH … WHERE
		// EXISTS. Its loop variable `b` — and everything its body binds — are
		// scoped to the body: Foreach.Vars() is documented as "FOREACH
		// introduces no variable visible after it; downstream sees exactly the
		// outer variables". So the subquery's `b` is a fresh binding and that
		// subquery is legally UNCORRELATED, structurally identical to S2.
		//
		// Expectation derived from control A, not from the pre-fix output: an
		// uncorrelated EXISTS is true for every outer row, so both rows ship.
		// The pre-fix build returned 0 rows here, because the leaked `b`
		// resolved against an absent slot.
		//
		// The TCK cannot catch this: 0 of its 220 feature files mention
		// FOREACH.
		got := run(t, `MATCH (a:P) FOREACH (b IN [1] | CREATE (:Q)) `+
			`WITH a WHERE EXISTS { MATCH (b)-[:Z]->(:P) } RETURN a.sid AS sid`)
		if !slices.Equal(got, wantBoth) {
			t.Errorf("FOREACH's loop variable leaked into the EXISTS correlation set, so the subquery was wrongly "+
				"correlated instead of uncorrelated\n got: %v\nwant: %v (control A: an uncorrelated EXISTS ships every row)",
				got, wantBoth)
		}
	})

	t.Run("S4ctrl_FOREACH_does_not_disturb_a_real_correlation", func(t *testing.T) {
		// The complement of S4: with the SAME FOREACH in place, a subquery that
		// correlates on the genuine outer variable `a` must still be selective.
		// Without this, S4 would be satisfied by a build that simply stopped
		// correlating anything after a FOREACH.
		got := run(t, `MATCH (a:P) FOREACH (b IN [1] | CREATE (:Q)) `+
			`WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN a.sid AS sid`)
		if !slices.Equal(got, wantOne) {
			t.Errorf("a genuine correlation on `a` was lost across a FOREACH\n got: %v\nwant: %v (control B)", got, wantOne)
		}
	})

	t.Run("S3_write_then_scope_cut_stays_correlated", func(t *testing.T) {
		// Control C without the write clause answers 1 row. The write clause
		// creates an unrelated :Q node and touches neither `a` nor any :Z edge,
		// so it CANNOT change the answer. That derivation — not the historical
		// output — is what fixes this expectation at 1.
		//
		// Recorded deliberately: the pre-fix build answered 2 here, and 2 is
		// WRONG. It is the #2659 symptom itself (the subquery lost its
		// correlation and fired for every outer row). This subtest is the one
		// place in the battery where "restore the old answer" would have
		// re-introduced the defect.
		got := run(t, `MATCH (a:P) CREATE (:Q) WITH 1 AS x WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN x`)
		if len(got) != len(scopeCutControl) {
			t.Errorf("an unrelated CREATE changed the answer: got %d rows, want %d (control C, same query without the write clause)\n got: %v",
				len(got), len(scopeCutControl), got)
		}
		if len(got) != 1 {
			t.Errorf("expected 1 row (correlated on `a`); 2 rows is the #2659 symptom, not the correct answer\n got: %v", got)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// The narrowing matrix
// ─────────────────────────────────────────────────────────────────────────────

// existsWriteVariant is one row of the narrowing matrix.
type existsWriteVariant struct {
	name  string
	setup string
	query string

	// wantRows is the exact expected row set, rendered and sorted. Used for
	// every variant whose output is deterministic across runs.
	wantRows []string

	// wantRowCount and wantNonNull replace wantRows for the two variants that
	// project node identity, which is not stable across runs. The row count
	// still pins the filter, and the non-null check still pins the binding.
	wantRowCount int
	wantNonNull  []string

	// wantErrContains, when non-empty, pins a variant that currently fails.
	// This is deliberate: the two variants that carry it are blocked on rmp
	// #2660 (no PatternEvaluator / SubqueryEvaluator wired on the write path),
	// which is filed separately and is NOT fixed here. When #2660 lands, these
	// two expectations must be replaced with the correct row sets — the failure
	// this produces is the intended signal, not a regression.
	wantErrContains string

	// why records what this variant discriminates, so a future reader can tell
	// which variants are load-bearing and which are controls.
	why string
}

// TestExistsAfterWriteMatrix pins all 24 variants of the #2659 narrowing
// matrix. The variants that were already correct before the fix are pinned so
// the fix cannot break them; the variants that were wrong are pinned to the
// openCypher-correct answer.
//
// The expectation for the load-bearing selective variants is not asserted from
// the specification alone: V19a pins the number of :Z edges the setup actually
// created, and V19b runs the identical predicate with no write clause. Together
// they establish that "2" is the right answer for V19 and V19c empirically.
func TestExistsAfterWriteMatrix(t *testing.T) {
	const n = 4
	all4 := sidRows(existsWriteBaseSID, existsWriteBaseSID+1, existsWriteBaseSID+2, existsWriteBaseSID+3)
	// selectiveSetup gives a :Z self-edge to the first two nodes only.
	selectiveSetup := fmt.Sprintf(`MATCH (a:P) WHERE a.sid < %d CREATE (a)-[:Z]->(a)`, existsWriteBaseSID+2)
	// allSetup gives a :Z self-edge to every node.
	const allSetup = `MATCH (a:P) CREATE (a)-[:Z]->(a)`

	variants := []existsWriteVariant{
		{
			name:     "V0_sanity_sid_is_set",
			query:    `MATCH (a:P) RETURN a.sid AS sid`,
			wantRows: all4,
			why:      "control: sid really is set, so a null elsewhere is a defect and not an unset property",
		},
		{
			name:     "V1_full_repro",
			query:    `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN a.sid AS sid`,
			wantRows: all4,
			why:      "the original report: every node gains a :Z self-edge, so every node satisfies the EXISTS",
		},
		{
			name:     "V2_no_create_readonly",
			setup:    allSetup,
			query:    `MATCH (a:P) WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN a.sid AS sid`,
			wantRows: all4,
			why:      "removing the write clause cured the defect: pins the read-only path",
		},
		{
			name:     "V3_no_exists",
			query:    `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a RETURN a.sid AS sid`,
			wantRows: all4,
			why:      "removing the EXISTS cured the defect: WITH after a write is fine on its own",
		},
		{
			name:     "V4_no_with",
			query:    `MATCH (a:P) CREATE (a)-[:Z]->(a) RETURN a.sid AS sid`,
			wantRows: all4,
			why:      "removing the WITH cured the defect: pins the no-scope-hop path",
		},
		{
			name:     "V5_with_plain_where",
			query:    `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a WHERE a.sid > 0 RETURN a.sid AS sid`,
			wantRows: all4,
			why:      "a non-subquery WHERE on the same WITH was always correct: the subquery is the trigger, not WITH+WHERE",
		},
		{
			name:            "V6_with_pattern_predicate",
			query:           `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a WHERE (a)-[:Z]->(:P) RETURN a.sid AS sid`,
			wantErrContains: "no PatternEvaluator",
			why:             "blocked on rmp #2660, filed separately and NOT fixed here; replace with all4 when #2660 lands",
		},
		{
			name:         "V7_return_whole_node",
			query:        `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN a`,
			wantRowCount: 4,
			wantNonNull:  []string{"a"},
			why:          "RETURN a was null too, so the BINDING was lost and not merely the property read",
		},
		{
			name:         "V8_return_id_and_sid",
			query:        `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN id(a) AS id, a.sid AS sid`,
			wantRowCount: 4,
			wantNonNull:  []string{"id", "sid"},
			why:          "id(a) was null as well: confirms the loss is of the binding, across two different derivations",
		},
		{
			name:     "V9_set_not_create",
			setup:    allSetup,
			query:    `MATCH (a:P) SET a.touched = true WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN a.sid AS sid`,
			wantRows: all4,
			why:      "SET was always correct because SetProperty.Vars() names the entity it sets; pins that path",
		},
		{
			name:     "V10_with_star",
			query:    `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH * WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN a.sid AS sid`,
			wantRows: all4,
			why:      "WITH * broke identically to WITH a: the projection form is not the trigger",
		},
		{
			name:     "V11_count_star_control",
			query:    `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN count(*) AS c`,
			wantRows: []string{"c=4"},
			why:      "non-selective count control: with every node edged, 4 is correct both correlated and not",
		},
		{
			name:     "V12_create_other_type",
			setup:    allSetup,
			query:    `MATCH (a:P) CREATE (a)-[:Z2]->(a) WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN a.sid AS sid`,
			wantRows: all4,
			why:      "the EXISTS reads an edge type the statement did not create: the defect is NOT about reading own writes",
		},
		{
			name:     "V13_create_unrelated_node",
			setup:    allSetup,
			query:    `MATCH (a:P) CREATE (:Q) WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN a.sid AS sid`,
			wantRows: all4,
			why:      "the CREATE need not touch `a` at all: any entity-creating clause was enough",
		},
		{
			name:     "V14_merge_not_create",
			setup:    allSetup,
			query:    `MATCH (a:P) MERGE (a)-[:Z3]->(a) WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN a.sid AS sid`,
			wantRows: all4,
			why:      "MERGE broke the same way as CREATE: the trigger is the class of clause, not the keyword",
		},
		{
			name:            "V15_exists_in_projection",
			query:           `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a RETURN a.sid AS sid, EXISTS { MATCH (a)-[:Z]->(:P) } AS ex`,
			wantErrContains: "no SubqueryEvaluator",
			why:             "blocked on rmp #2660, filed separately and NOT fixed here; replace with real rows when #2660 lands",
		},
		{
			name:     "V16_uncorrelated_exists",
			setup:    allSetup,
			query:    `MATCH (a:P) CREATE (a)-[:Z4]->(a) WITH a WHERE EXISTS { MATCH (:P)-[:Z]->(:P) } RETURN a.sid AS sid`,
			wantRows: all4,
			why:      "an uncorrelated EXISTS was always correct: correlation on `a` is required to trigger the defect",
		},
		{
			name:     "V17_two_with_hops",
			query:    `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN a.sid AS sid`,
			wantRows: all4,
			why:      "a second WITH cured it, because the outer plan's top operator became a Projection naming `a`",
		},
		{
			name:     "V18_two_vars",
			query:    `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a, a.sid AS keep WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN keep, a.sid AS sid`,
			wantRows: sortedRows("keep=100000 sid=100000", "keep=100001 sid=100001", "keep=100002 sid=100002", "keep=100003 sid=100003"),
			why:      "an entity-derived alias computed AT the WITH was null too, so the loss precedes the projection",
		},
		{
			name:     "V19_selective_predicate",
			setup:    selectiveSetup,
			query:    `MATCH (a:P) CREATE (:Q) WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN count(*) AS c`,
			wantRows: []string{"c=2"},
			why:      "LOAD-BEARING: only 2 of 4 nodes are edged, so a count of 4 proves the filter lost its correlation. A null cannot fake this number",
		},
		{
			name:     "V20_literal_alongside",
			query:    `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a, 42 AS lit WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN lit, a.sid AS sid`,
			wantRows: sortedRows("lit=42 sid=100000", "lit=42 sid=100001", "lit=42 sid=100002", "lit=42 sid=100003"),
			why:      "the non-entity value survived while the entity-derived one did not: pins that both now survive",
		},
		{
			name:     "V19a_control_edge_count",
			setup:    selectiveSetup,
			query:    `MATCH ()-[r:Z]->() RETURN count(r) AS c`,
			wantRows: []string{"c=2"},
			why:      "CONTROL for V19: proves the selective setup created 2 edges, not 4. Without it, V19's expected 2 is an assumption",
		},
		{
			name:     "V19b_control_readonly_selective",
			setup:    selectiveSetup,
			query:    `MATCH (a:P) WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN count(*) AS c`,
			wantRows: []string{"c=2"},
			why:      "CONTROL for V19: the identical predicate with no write clause also answers 2, establishing the expectation empirically",
		},
		{
			// NOT EXISTS shares existsSubPlan with EXISTS, so it shares the
			// defect and the fix. Its expectation is the exact complement of
			// V19c over the same setup, which makes the pair a two-sided
			// oracle: a build that returned all four rows would satisfy
			// neither, and one that returned none would satisfy neither.
			name:     "V21_not_exists_selective",
			setup:    selectiveSetup,
			query:    `MATCH (a:P) CREATE (:Q) WITH a WHERE NOT EXISTS { MATCH (a)-[:Z]->(:P) } RETURN a.sid AS sid`,
			wantRows: sidRows(existsWriteBaseSID+2, existsWriteBaseSID+3),
			why:      "LOAD-BEARING: AntiSemiApply takes the same correlation seed; the complement of V19c pins it from the other side",
		},
		{
			name:     "V19c_selective_sids_not_count",
			setup:    selectiveSetup,
			query:    `MATCH (a:P) CREATE (:Q) WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN a.sid AS sid`,
			wantRows: sidRows(existsWriteBaseSID, existsWriteBaseSID+1),
			why:      "LOAD-BEARING: pins the row set AND the values together — exactly the two edged nodes, with their real sids",
		},
	}

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			e := newExistsWriteFixture(t, n)
			if v.setup != "" {
				if _, err := e.RunAny(context.Background(), v.setup, nil); err != nil {
					t.Fatalf("setup %q: %v", v.setup, err)
				}
			}
			got, errText := drainExistsWrite(t, e, v.query)

			if v.wantErrContains != "" {
				if !strings.Contains(errText, v.wantErrContains) {
					t.Fatalf("expected an error containing %q (rmp #2660 — see wantErrContains), got error %q and rows %v",
						v.wantErrContains, errText, got)
				}
				return
			}
			if errText != "" {
				t.Fatalf("unexpected error: %s", errText)
			}

			if v.wantRows != nil {
				if !slices.Equal(got, v.wantRows) {
					t.Errorf("wrong result (%s)\n got: %v\nwant: %v", v.why, got, v.wantRows)
				}
				return
			}

			// Identity-projecting variants: pin the row count and non-nullness.
			if len(got) != v.wantRowCount {
				t.Errorf("wrong row count (%s): got %d, want %d\n got: %v", v.why, len(got), v.wantRowCount, got)
			}
			for _, row := range got {
				for _, col := range v.wantNonNull {
					if strings.Contains(row, col+"=null") {
						t.Errorf("column %q is null (%s): row %q", col, v.why, row)
					}
				}
			}
		})
	}
}

// sortedRows returns rows sorted, so a literal expectation in the table above
// does not have to be written in scan order.
func sortedRows(rows ...string) []string {
	out := slices.Clone(rows)
	slices.Sort(out)
	return out
}
