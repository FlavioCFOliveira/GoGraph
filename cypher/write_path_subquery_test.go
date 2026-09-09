package cypher_test

// write_path_subquery_test.go — regression battery for rmp #2660: three
// expression-level constructs that a read-only statement evaluates were REFUSED
// once the statement wrote.
//
// Each failure was a typed error, not a wrong answer:
//
//	MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a WHERE (a)-[:Z]->(:P) RETURN a.sid
//	  → eval: pattern predicate is not supported in this evaluation context
//	          (no PatternEvaluator wired)
//
//	MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a RETURN a.sid, EXISTS { … } AS ex
//	  → exec: Project item "ex" eval: eval: EXISTS { … } subquery is not
//	          supported in this evaluation context (no SubqueryEvaluator wired)
//
//	MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a RETURN a.sid, COUNT { … } AS c
//	  → the same, for COUNT { … }
//
// # One omission, two fields
//
// All three trace to ONE site: [buildPlanWithMutatorFull] built its buildOpts as
// `&buildOpts{maxCollectItems: …, procReg: …}`, leaving subEval and patEval nil,
// so [evalRow] took its `both nil` branch and degraded to the bare [expr.Eval]
// path for EVERY expression in a statement that writes. The two fields are
// distinct and each carries a different subset of the symptoms — wiring only
// patEval fixes the pattern predicate and leaves both subqueries erroring, and
// wiring only subEval does the converse — but neither was ever set, by the same
// expression, so the three symptoms have a single root cause. The read path has
// wired both since task-396 / task-961 ([Engine.buildReadPhysical]).
//
// # What every case asserts
//
// A writing statement and its read-only equivalent are run against the SAME
// fixture state and their row sets compared. That is the acceptance criterion
// stated literally: the construct must evaluate on the write path exactly as it
// does on the read path. Comparing against the read path rather than against a
// hand-written expectation is deliberate — a hard-coded row set would still pass
// on a build where BOTH paths had drifted.
//
// The fixture gives each node a SELF-edge, so the answer does not depend on
// whether the write pipeline is eager: row i observes only the edge row i just
// created either way. A fixture whose write affected OTHER rows would be
// measuring pipelining, not this fix.

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

// wpsBaseSID is the first sid handed to the fixture's :P nodes. Large and
// distinct, so a null, a zero or a node id can never be mistaken for one.
const wpsBaseSID = 700000

// wpsNodes is the fixture size. Four is enough for a selective predicate to
// exclude some rows and keep others.
const wpsNodes = 4

// newWPSFixture builds n :P nodes carrying sid = wpsBaseSID+i and NO
// relationships, so every edge the cases rely on is created by the statement
// under test or by an explicit setup — never by the fixture.
func newWPSFixture(t *testing.T, n int) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	e := cypher.NewEngine(g)
	for i := range n {
		if _, err := e.RunAny(context.Background(), fmt.Sprintf(`CREATE (:P {sid:%d})`, wpsBaseSID+i), nil); err != nil {
			t.Fatalf("fixture node %d: %v", i, err)
		}
	}
	return e
}

// wpsDrain runs q and returns one rendered row per result row, sorted, plus the
// error text. Errors from RunAny and from the lazy stream are reported the same
// way, because #2660 surfaced through both.
func wpsDrain(t *testing.T, e *cypher.Engine, q string) (rows []string, errText string) {
	t.Helper()
	res, err := e.RunAny(context.Background(), q, nil)
	if err != nil {
		return nil, err.Error()
	}
	for res.Next() {
		rows = append(rows, wpsRender(res.Record()))
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

// wpsRender formats a record as "k=v" pairs in key order.
func wpsRender(rec map[string]any) string {
	keys := make([]string, 0, len(rec))
	for k := range rec {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+wpsValue(rec[k]))
	}
	return strings.Join(parts, " ")
}

func wpsValue(v any) string {
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
		return fmt.Sprintf("%v", v)
	}
}

// wpsCase is one write-path construct paired with the read-only statement that
// must produce the identical row set.
type wpsCase struct {
	name string
	// setup runs against BOTH arms before the statement under test, so the two
	// observe the same starting graph.
	setup string
	// write is the statement under test: it writes, then uses the construct.
	write string
	// readSetup performs, as its own statement, exactly the write that `write`
	// performs inline, so the read-only arm observes the same graph. It runs on
	// the read arm ONLY.
	readSetup string
	// read is the read-only equivalent of `write`, minus the write clause.
	read string
	// wantRowCount pins the size of the shared answer, so a build in which BOTH
	// arms collapsed to zero rows cannot pass by agreeing with itself.
	wantRowCount int
	// why records which construct this case discriminates, and whether it is a
	// gate or a control.
	why string
}

// TestWritePathSubqueryAndPatternPredicate is the acceptance battery for
// rmp #2660. With the fix neutralised, 15 of its 16 cases fail with a typed
// "not supported in this evaluation context" error and no rows at all; the
// sixteenth is declared a control in its own `why` and passes either way.
func TestWritePathSubqueryAndPatternPredicate(t *testing.T) {
	all := wpsNodes
	const selfEdges = `MATCH (a:P) CREATE (a)-[:Z]->(a)`
	// halfEdges gives a :Z self-edge to the FIRST TWO nodes only, so a case
	// built on it has a per-row answer rather than a uniform one.
	halfEdges := fmt.Sprintf(`MATCH (a:P) WHERE a.sid < %d CREATE (a)-[:Z]->(a)`, wpsBaseSID+2)

	cases := []wpsCase{
		// ── Construct 1: a bare pattern predicate (patEval) ──────────────────
		{
			name:         "pattern_predicate_in_with_where",
			write:        `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a WHERE (a)-[:Z]->(:P) RETURN a.sid AS sid`,
			readSetup:    selfEdges,
			read:         `MATCH (a:P) WITH a WHERE (a)-[:Z]->(:P) RETURN a.sid AS sid`,
			wantRowCount: all,
			why:          "the exact statement in the #2660 report: patEval was nil, so this raised 'no PatternEvaluator wired'",
		},
		{
			name:         "pattern_predicate_negated",
			write:        `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a WHERE NOT (a)-[:Q]->(:P) RETURN a.sid AS sid`,
			readSetup:    selfEdges,
			read:         `MATCH (a:P) WITH a WHERE NOT (a)-[:Q]->(:P) RETURN a.sid AS sid`,
			wantRowCount: all,
			why:          "NOT of a pattern predicate reaches the same evaluator; a nil patEval failed it too",
		},
		{
			name:         "degree_countable_size_comprehension",
			write:        `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a RETURN a.sid AS sid, size([(a)-[:Z]->() | 1]) AS s`,
			readSetup:    selfEdges,
			read:         `MATCH (a:P) WITH a RETURN a.sid AS sid, size([(a)-[:Z]->() | 1]) AS s`,
			wantRowCount: all,
			why:          "A FOURTH SYMPTOM, not in the #2660 report and found while closing it: the degree-countable `size(<comprehension>)` is deliberately LEFT UNHOISTED so the runtime can answer it from the adjacency (rmp #2264), which means it needs patEval — and on the write path it failed with 'pattern comprehension is not supported in this evaluation context'",
		},
		{
			name:         "degree_countable_size_in_where",
			write:        `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a WHERE size([(a)-[:Z]->() | 1]) = 1 RETURN a.sid AS sid`,
			readSetup:    selfEdges,
			read:         `MATCH (a:P) WITH a WHERE size([(a)-[:Z]->() | 1]) = 1 RETURN a.sid AS sid`,
			wantRowCount: all,
			why:          "the same fourth symptom in filter position",
		},
		{
			name:         "hoisted_comprehension_control",
			write:        `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a RETURN a.sid AS sid, size([(a)-[:Z]->(x:P) | x]) AS s`,
			readSetup:    selfEdges,
			read:         `MATCH (a:P) WITH a RETURN a.sid AS sid, size([(a)-[:Z]->(x:P) | x]) AS s`,
			wantRowCount: all,
			why:          "CONTROL, and MEASURED as one: this spelling is not degree-countable, so the translator hoists it to a RollUpApply that needs no evaluator. It passes with the fix neutralised — it is here to mark the boundary of the defect, not to gate it",
		},

		// ── Construct 2: EXISTS { … } outside WHERE position (subEval) ───────
		{
			name:         "exists_in_projection",
			write:        `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a RETURN a.sid AS sid, EXISTS { MATCH (a)-[:Z]->(:P) } AS ex`,
			readSetup:    selfEdges,
			read:         `MATCH (a:P) WITH a RETURN a.sid AS sid, EXISTS { MATCH (a)-[:Z]->(:P) } AS ex`,
			wantRowCount: all,
			why:          "the second statement in the #2660 report: no SemiApply lowering applies in projection position, so subEval had to answer it",
		},
		{
			name:         "exists_in_composite_predicate",
			write:        `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } AND a.sid >= 0 RETURN a.sid AS sid`,
			readSetup:    selfEdges,
			read:         `MATCH (a:P) WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } AND a.sid >= 0 RETURN a.sid AS sid`,
			wantRowCount: all,
			why:          "an EXISTS conjoined with another term is no longer a TOP-LEVEL EXISTS, so it falls out of the SemiApply rewrite and back onto subEval",
		},
		{
			name:         "exists_in_case_branch",
			write:        `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a RETURN a.sid AS sid, CASE WHEN EXISTS { MATCH (a)-[:Z]->(:P) } THEN 1 ELSE 0 END AS c`,
			readSetup:    selfEdges,
			read:         `MATCH (a:P) WITH a RETURN a.sid AS sid, CASE WHEN EXISTS { MATCH (a)-[:Z]->(:P) } THEN 1 ELSE 0 END AS c`,
			wantRowCount: all,
			why:          "a CASE branch is a third position with no rewrite available",
		},

		// ── Construct 3: COUNT { … }, which has no lowering anywhere ─────────
		{
			name:         "count_in_projection",
			write:        `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a RETURN a.sid AS sid, COUNT { MATCH (a)-[:Z]->(:P) } AS c`,
			readSetup:    selfEdges,
			read:         `MATCH (a:P) WITH a RETURN a.sid AS sid, COUNT { MATCH (a)-[:Z]->(:P) } AS c`,
			wantRowCount: all,
			why:          "the third statement in the #2660 report",
		},
		{
			name:         "count_in_where_comparison",
			write:        `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a WHERE COUNT { MATCH (a)-[:Z]->(:P) } = 1 RETURN a.sid AS sid`,
			readSetup:    selfEdges,
			read:         `MATCH (a:P) WITH a WHERE COUNT { MATCH (a)-[:Z]->(:P) } = 1 RETURN a.sid AS sid`,
			wantRowCount: all,
			why:          "WHERE position: #2660 recorded COUNT failing in EVERY placement tried, so the filter spelling is pinned alongside the projection one",
		},
		{
			name:         "count_pattern_form_in_projection",
			write:        `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a RETURN a.sid AS sid, COUNT { (a)-[:Z]->(:P) } AS c`,
			readSetup:    selfEdges,
			read:         `MATCH (a:P) WITH a RETURN a.sid AS sid, COUNT { (a)-[:Z]->(:P) } AS c`,
			wantRowCount: all,
			why:          "the pattern spelling of COUNT { }, which reaches a different recogniser from the MATCH block form",
		},
		{
			name:         "count_on_set_rhs",
			write:        `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a SET a.deg = COUNT { MATCH (a)-[:Z]->(:P) } WITH a RETURN a.sid AS sid, a.deg AS deg`,
			readSetup:    selfEdges,
			read:         `MATCH (a:P) WITH a RETURN a.sid AS sid, 1 AS deg`,
			wantRowCount: all,
			why:          "a COUNT on a SET right-hand side used to be swallowed into a fail-stop error; the read arm pins the stored value against the literal 1 the fixture makes true",
		},

		// ── Selective controls: an answer that is NOT uniform ────────────────
		//
		// Every case above answers the same thing for all four rows, so a build
		// that ignored the correlation entirely would still satisfy them. These
		// two make the answer differ per row: only the first two nodes get an
		// edge, so a lost correlation shows up as four rows instead of two, or
		// as a uniform count.
		{
			name:         "selective_pattern_predicate",
			write:        `MATCH (a:P) WHERE a.sid < 700002 CREATE (a)-[:Z]->(a) WITH a WHERE (a)-[:Z]->(:P) RETURN a.sid AS sid`,
			readSetup:    `MATCH (a:P) WHERE a.sid < 700002 CREATE (a)-[:Z]->(a)`,
			read:         `MATCH (a:P) WHERE a.sid < 700002 WITH a WHERE (a)-[:Z]->(:P) RETURN a.sid AS sid`,
			wantRowCount: 2,
			why:          "LOAD-BEARING: the predicate must reject the two nodes with no edge, so a correlation-blind build cannot pass",
		},
		{
			name:         "selective_count_projection",
			setup:        halfEdges,
			write:        `MATCH (a:P) SET a.touched = true WITH a RETURN a.sid AS sid, COUNT { MATCH (a)-[:Z]->(:P) } AS c`,
			read:         `MATCH (a:P) WITH a RETURN a.sid AS sid, COUNT { MATCH (a)-[:Z]->(:P) } AS c`,
			wantRowCount: all,
			why:          "LOAD-BEARING: the counted edges exist only for HALF the nodes, so the answer differs per row (1,1,0,0) and a correlation-blind build cannot pass. The write is a SET, which does not touch the counted pattern, so the case is insensitive to whether the pipeline is eager",
		},
		{
			name:         "selective_exists_projection",
			setup:        halfEdges,
			write:        `MATCH (a:P) SET a.touched = true WITH a RETURN a.sid AS sid, EXISTS { MATCH (a)-[:Z]->(:P) } AS ex`,
			read:         `MATCH (a:P) WITH a RETURN a.sid AS sid, EXISTS { MATCH (a)-[:Z]->(:P) } AS ex`,
			wantRowCount: all,
			why:          "the EXISTS half of the same discrimination: two rows must answer true and two false",
		},
		{
			name:         "selective_pattern_predicate_after_set",
			setup:        halfEdges,
			write:        `MATCH (a:P) SET a.touched = true WITH a WHERE (a)-[:Z]->(:P) RETURN a.sid AS sid`,
			read:         `MATCH (a:P) WITH a WHERE (a)-[:Z]->(:P) RETURN a.sid AS sid`,
			wantRowCount: 2,
			why:          "the patEval half: only the two edged nodes survive the filter",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			we := newWPSFixture(t, wpsNodes)
			if c.setup != "" {
				if _, err := we.RunAny(context.Background(), c.setup, nil); err != nil {
					t.Fatalf("write-arm setup %q: %v", c.setup, err)
				}
			}
			gotWrite, writeErr := wpsDrain(t, we, c.write)
			if writeErr != "" {
				t.Fatalf("write statement failed (%s): %s\n  %s", c.why, writeErr, c.write)
			}

			re := newWPSFixture(t, wpsNodes)
			if c.setup != "" {
				if _, err := re.RunAny(context.Background(), c.setup, nil); err != nil {
					t.Fatalf("read-arm setup %q: %v", c.setup, err)
				}
			}
			if c.readSetup != "" {
				if _, err := re.RunAny(context.Background(), c.readSetup, nil); err != nil {
					t.Fatalf("read-arm write replay %q: %v", c.readSetup, err)
				}
			}
			gotRead, readErr := wpsDrain(t, re, c.read)
			if readErr != "" {
				t.Fatalf("read-only equivalent failed (%s): %s\n  %s", c.why, readErr, c.read)
			}

			// The row count is pinned FIRST, so two arms that agree on the empty
			// set cannot pass by agreeing with each other.
			if len(gotRead) != c.wantRowCount {
				t.Fatalf("read-only arm produced %d rows, want %d (%s)\n got: %v",
					len(gotRead), c.wantRowCount, c.why, gotRead)
			}
			if !slices.Equal(gotWrite, gotRead) {
				t.Errorf("write path disagrees with its read-only equivalent (%s)\n write: %v\n  read: %v",
					c.why, gotWrite, gotRead)
			}
		})
	}
}

// TestWritePathSubquery_ExplicitTx pins the SAME three constructs on the OTHER
// write entry point. Both reach the plan build through Engine.execUnderBarrier,
// but by different callers — RunInTx commits per statement, an explicit
// transaction's Exec does not — and only the autocommit one is exercised above.
// A fix wired on one and not the other would pass every case in this file
// except this one.
func TestWritePathSubquery_ExplicitTx(t *testing.T) {
	ctx := context.Background()
	e := newWPSFixture(t, wpsNodes)

	tx, err := e.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecAny(
		`MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a RETURN a.sid AS sid, EXISTS { MATCH (a)-[:Z]->(:P) } AS ex, COUNT { MATCH (a)-[:Z]->(:P) } AS c`, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	var rows []string
	for res.Next() {
		rows = append(rows, wpsRender(res.Record()))
	}
	if iterErr := res.Err(); iterErr != nil {
		t.Fatalf("explicit-tx statement failed: %v", iterErr)
	}
	_ = res.Close()
	slices.Sort(rows)

	want := []string{
		"c=1 ex=true sid=700000",
		"c=1 ex=true sid=700001",
		"c=1 ex=true sid=700002",
		"c=1 ex=true sid=700003",
	}
	if !slices.Equal(rows, want) {
		t.Errorf("explicit-tx write path\n got: %v\nwant: %v", rows, want)
	}
}

// TestWritePathSubquery_SeesOwnWrites is the discriminating half of the fix: it
// is not enough that the three constructs run, they must read through the
// WRITING transaction's own view. A build that handed the evaluators a
// committed read view instead would pass every case above — the fixture's
// self-edges make the two views agree once the statement commits — and fail
// here, because these statements ask about a graph that only exists mid-flight.
func TestWritePathSubquery_SeesOwnWrites(t *testing.T) {
	cases := []struct {
		name  string
		setup string
		q     string
		want  []string
		why   string
	}{
		{
			name:  "create_is_visible",
			setup: "",
			q:     `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a RETURN a.sid AS sid, COUNT { MATCH (a)-[:Z]->(:P) } AS c`,
			want: []string{
				"c=1 sid=700000", "c=1 sid=700001", "c=1 sid=700002", "c=1 sid=700003",
			},
			why: "an evaluator reading a committed view would answer 0: the edge does not exist outside this transaction yet",
		},
		{
			name:  "delete_is_visible",
			setup: `MATCH (a:P) CREATE (a)-[:Z]->(a)`,
			q:     `MATCH (a:P)-[r:Z]->(a) DELETE r WITH a RETURN a.sid AS sid, COUNT { MATCH (a)-[:Z]->(:P) } AS c`,
			want: []string{
				"c=0 sid=700000", "c=0 sid=700001", "c=0 sid=700002", "c=0 sid=700003",
			},
			why: "the mirror image: an evaluator reading a committed view would still see the deleted edge and answer 1",
		},
		{
			name:  "delete_is_visible_to_pattern_predicate",
			setup: `MATCH (a:P) CREATE (a)-[:Z]->(a)`,
			q:     `MATCH (a:P)-[r:Z]->(a) DELETE r WITH a WHERE (a)-[:Z]->(:P) RETURN a.sid AS sid`,
			want:  nil,
			why:   "same discrimination through patEval rather than subEval: every row must be rejected",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newWPSFixture(t, wpsNodes)
			if c.setup != "" {
				if _, err := e.RunAny(context.Background(), c.setup, nil); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}
			got, errText := wpsDrain(t, e, c.q)
			if errText != "" {
				t.Fatalf("statement failed (%s): %s", c.why, errText)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("wrong answer (%s)\n got: %v\nwant: %v", c.why, got, c.want)
			}
		})
	}
}

// TestWritePathSubquery_AdjacencyRewriteAgrees is a differential gate on the
// two adjacency-answered shortcuts the evaluators may take
// (EngineOptions.DisableAdjacencyCountRewrites, #2232 / #2235). Both answer
// EXISTS / COUNT / a pattern predicate from the adjacency instead of driving an
// inner plan, and both now run against a WRITER view for the first time — a
// view carrying uncommitted work, which is precisely the condition the
// adjacency cache refuses to serve. If either shortcut read a structure that
// did not carry this statement's own writes, it would produce a WRONG ANSWER
// rather than an error, which is strictly worse than the defect this task
// closes. Measured: with the rewrites enabled these statements take them 4
// times each (once per row) and 0 times with them disabled.
func TestWritePathSubquery_AdjacencyRewriteAgrees(t *testing.T) {
	cases := []struct{ name, setup, q string }{
		{"count_labelled_hop", "", `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a RETURN a.sid AS sid, COUNT { MATCH (a)-[:Z]->(:P) } AS c`},
		{"count_typed_degree", "", `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a RETURN a.sid AS sid, COUNT { MATCH (a)-[:Z]->() } AS c`},
		{"count_bare_degree", "", `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a RETURN a.sid AS sid, COUNT { MATCH (a)-->() } AS c`},
		{"exists_labelled_hop", "", `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a RETURN a.sid AS sid, EXISTS { MATCH (a)-[:Z]->(:P) } AS ex`},
		{"pattern_predicate", "", `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a WHERE (a)-[:Z]->(:P) RETURN a.sid AS sid`},
		{"size_comprehension", "", `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a RETURN a.sid AS sid, size([(a)-[:Z]->() | 1]) AS s`},
		{"count_after_delete", `MATCH (a:P) CREATE (a)-[:Z]->(a)`, `MATCH (a:P)-[r:Z]->(a) DELETE r WITH a RETURN a.sid AS sid, COUNT { MATCH (a)-[:Z]->(:P) } AS c`},
		{"exists_after_delete", `MATCH (a:P) CREATE (a)-[:Z]->(a)`, `MATCH (a:P)-[r:Z]->(a) DELETE r WITH a RETURN a.sid AS sid, EXISTS { MATCH (a)-[:Z]->() } AS ex`},
	}
	arm := func(t *testing.T, disable bool, setup, q string) []string {
		t.Helper()
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		e := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableAdjacencyCountRewrites: disable})
		for i := range wpsNodes {
			if _, err := e.RunAny(context.Background(), fmt.Sprintf(`CREATE (:P {sid:%d})`, wpsBaseSID+i), nil); err != nil {
				t.Fatalf("fixture: %v", err)
			}
		}
		if setup != "" {
			if _, err := e.RunAny(context.Background(), setup, nil); err != nil {
				t.Fatalf("setup: %v", err)
			}
		}
		rows, errText := wpsDrain(t, e, q)
		if errText != "" {
			t.Fatalf("statement failed (rewrites disabled=%t): %s", disable, errText)
		}
		return rows
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			on := arm(t, false, c.setup, c.q)
			off := arm(t, true, c.setup, c.q)
			if len(on) == 0 && len(off) == 0 {
				t.Fatalf("both arms empty: the case discriminates nothing")
			}
			if !slices.Equal(on, off) {
				t.Errorf("the adjacency-answered rewrite disagrees with the inner plan on the write path\n rewrite on : %v\n rewrite off: %v", on, off)
			}
		})
	}
}
