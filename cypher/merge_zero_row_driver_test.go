package cypher_test

// merge_zero_row_driver_test.go — regression coverage for rmp #2512: MERGE fired
// against an empty row when its DRIVING CLAUSE produced no rows, creating data no
// statement asked for.
//
//	MATCH (m:M {name:'absent'}) MERGE (q:Q {name:'q'})
//
// reported +1 node / +1 property / +1 label and left a :Q node in the graph, on a
// statement whose MATCH bound nothing. The pattern form was worse — two nodes and
// a relationship. openCypher executes MERGE once per incoming row (openCypher 9
// §MERGE; TCK Merge1 "Merge node when no nodes exist" is the LEADING-clause case,
// and the drive-per-row rule is what makes `UNWIND [] AS x MERGE (…)` a no-op),
// so a driver producing zero rows must produce zero executions.
//
// Cause: [exec.Merge.Next] and [exec.MergePattern.Next] carried a
// `if !firedOnce { runForRow(Row{}) }` fallback on child exhaustion. It existed to
// make a LEADING-clause MERGE (`MERGE (a:Foo)` with no driver at all) fire once,
// but it could not tell that case apart from a MERGE whose child returned no rows
// — the operator sees an exhausted child either way. The leading-clause case never
// needed it: the physical builder already installs [exec.SingleRow] as the child of
// a write operator whose IR child is nil, and that leaf emits exactly one empty
// row. The fallback therefore only ever fired in the defective case.
//
// The distinction is now threaded from the plan builder (WithLeadingClause), so it
// is decided by plan shape rather than inferred at runtime from whether rows
// arrived.
//
// The matrix crosses driver cardinality (zero / leading-clause-none / one / many /
// null-padded) against both merge operators (node-only [exec.Merge], whole-pattern
// [exec.MergePattern]), because the defect and the fallback were duplicated in
// both.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// ─────────────────────────────────────────────────────────────────────────────
// Shared clause text
// ─────────────────────────────────────────────────────────────────────────────

const (
	// zeroRowNodeMerge is the node-only MERGE form, routed to [exec.Merge].
	zeroRowNodeMerge = `MERGE (q:Q {name:'q'})`
	// zeroRowPatternMerge is the whole-pattern form, routed to [exec.MergePattern].
	zeroRowPatternMerge = `MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'})`
)

// zeroRowNodeEffect / zeroRowPatternEffect are the side effects of ONE firing of
// each merge form against a graph that does not already hold the pattern.
var (
	zeroRowNodeEffect = wantCounters{
		nodesCreated: 1, propsSet: 1, labelsAdded: 1, containsUpdates: true,
	}
	zeroRowPatternEffect = wantCounters{
		nodesCreated: 2, relsCreated: 1, propsSet: 2, labelsAdded: 2, containsUpdates: true,
	}
)

// assertNothingMerged fails when either merge form left anything behind. Both
// forms are checked on every zero-row case regardless of which one the statement
// used, so a fix that repairs one operator and not the other cannot pass.
func assertNothingMerged(t *testing.T, eng *cypher.Engine, stmt string) {
	t.Helper()
	for _, probe := range []struct {
		what  string
		query string
	}{
		{":Q nodes", `MATCH (n:Q) RETURN n AS v`},
		{":P nodes", `MATCH (n:P) RETURN n AS v`},
		{":T relationships", `MATCH ()-[r:T]->() RETURN r AS v`},
	} {
		if n := outerRowCount(t, eng, probe.query); n != 0 {
			t.Errorf("%s: %d %s exist; a MERGE driven by zero rows must create nothing", stmt, n, probe.what)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Zero-row drivers — MERGE must not run at all
// ─────────────────────────────────────────────────────────────────────────────

// zeroRowDriver is a driving-clause prefix that yields no rows, paired with any
// graph state it needs. Each is crossed with both merge forms.
type zeroRowDriver struct {
	name   string
	setup  string
	prefix string
}

// zeroRowDrivers are the ways a driving clause reaches the MERGE with an empty
// stream. They differ in which operator exhausts first — a scan with an
// unsatisfiable property predicate, a post-WITH filter, an empty UNWIND list, a
// WHERE false on the scan — so a fix that only handles one shape is caught.
var zeroRowDrivers = []zeroRowDriver{
	{
		name:   "match_binds_nothing",
		setup:  `CREATE (:M {name:'present'})`,
		prefix: `MATCH (m:M {name:'absent'})`,
	},
	{
		name:   "match_then_with_passthrough",
		setup:  `CREATE (:M {name:'present'})`,
		prefix: `MATCH (m:M {name:'absent'}) WITH m`,
	},
	{
		name:   "with_where_false",
		setup:  `CREATE (:M {name:'present'})`,
		prefix: `MATCH (m:M) WITH m WHERE false`,
	},
	{
		name:   "leading_with_where_false",
		prefix: `WITH 1 AS x WHERE false`,
	},
	{
		name:   "unwind_empty_list",
		prefix: `UNWIND [] AS x`,
	},
	{
		name:   "match_where_false",
		setup:  `CREATE (:M {name:'present'})`,
		prefix: `MATCH (m:M) WHERE false`,
	},
}

// zeroRowMergeForm is one MERGE clause with the operator it routes to.
type zeroRowMergeForm struct {
	name   string
	clause string
}

var zeroRowMergeForms = []zeroRowMergeForm{
	{name: "node", clause: zeroRowNodeMerge},
	{name: "pattern", clause: zeroRowPatternMerge},
}

// TestMergeZeroRowDriver_CreatesNothing is the core matrix: every zero-row driver
// × both merge forms must report an all-zero effect set and leave the graph
// untouched. Before rmp #2512 every cell created the whole MERGE pattern.
func TestMergeZeroRowDriver_CreatesNothing(t *testing.T) {
	t.Parallel()
	for _, d := range zeroRowDrivers {
		for _, f := range zeroRowMergeForms {
			t.Run(d.name+"/"+f.name, func(t *testing.T) {
				t.Parallel()
				eng := setMapEng(t)
				if d.setup != "" {
					drainRunInTx(t, eng, d.setup)
				}
				stmt := d.prefix + " " + f.clause
				c := runCountedP(t, eng, stmt, nil)
				assertCounters(t, stmt, c, wantCounters{})
				assertNothingMerged(t, eng, stmt)
			})
		}
	}
}

// TestMergeZeroRowDriver_ActionsDoNotRun: ON CREATE and ON MATCH are attached to a
// MERGE that never executes, so neither branch may run. A fix that suppressed only
// the pattern write while still running the action list would pass the matrix above
// and fail here.
func TestMergeZeroRowDriver_ActionsDoNotRun(t *testing.T) {
	t.Parallel()
	for _, f := range zeroRowMergeForms {
		for _, branch := range []string{"ON CREATE", "ON MATCH"} {
			t.Run(f.name+"/"+branch[3:], func(t *testing.T) {
				t.Parallel()
				eng := setMapEng(t)
				drainRunInTx(t, eng, `CREATE (:M {name:'present'})`)
				stmt := `MATCH (m:M {name:'absent'}) ` + f.clause + ` ` + branch + ` SET m.w = 'V'`
				c := runCountedP(t, eng, stmt, nil)
				assertCounters(t, stmt, c, wantCounters{})
				assertNothingMerged(t, eng, stmt)
				if !setScalarIsNull(t, eng, `MATCH (m:M {name:'present'}) RETURN m.w AS v`) {
					t.Errorf("%s: an action on a MERGE that never ran must write nothing", stmt)
				}
			})
		}
	}
}

// TestMergeZeroRowDriver_ForeachEmptyList: FOREACH builds its body over an UNWIND
// of the loop expression, so an empty list is a zero-row driver reached by a
// different route — the body operators run inside a correlated sub-plan rather
// than the main pipeline.
func TestMergeZeroRowDriver_ForeachEmptyList(t *testing.T) {
	t.Parallel()
	for _, f := range zeroRowMergeForms {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			eng := setMapEng(t)
			stmt := `FOREACH (x IN [] | ` + f.clause + `)`
			c := runCountedP(t, eng, stmt, nil)
			assertCounters(t, stmt, c, wantCounters{})
			assertNothingMerged(t, eng, stmt)
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Leading clause — MERGE has no driver at all and must fire exactly once
// ─────────────────────────────────────────────────────────────────────────────

// TestMergeLeadingClause_FiresExactlyOnce pins the case the removed fallback
// claimed to serve. A MERGE with no preceding clause has exactly one incoming
// row — the synthetic empty row — so it runs once, and running it again matches
// rather than creates.
func TestMergeLeadingClause_FiresExactlyOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		clause  string
		want    wantCounters
		probes  []string
		wantNum []int
	}{
		{
			name: "node", clause: zeroRowNodeMerge, want: zeroRowNodeEffect,
			probes:  []string{`MATCH (n:Q) RETURN n AS v`},
			wantNum: []int{1},
		},
		{
			name: "pattern", clause: zeroRowPatternMerge, want: zeroRowPatternEffect,
			probes:  []string{`MATCH (n:P) RETURN n AS v`, `MATCH ()-[r:T]->() RETURN r AS v`},
			wantNum: []int{2, 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := setMapEng(t)
			c := runCountedP(t, eng, tc.clause, nil)
			assertCounters(t, tc.clause, c, tc.want)
			for i, p := range tc.probes {
				if n := outerRowCount(t, eng, p); n != tc.wantNum[i] {
					t.Fatalf("%s: %s returned %d rows, want %d", tc.clause, p, n, tc.wantNum[i])
				}
			}
			// Idempotence: the second execution matches what the first created.
			again := runCountedP(t, eng, tc.clause, nil)
			assertCounters(t, tc.clause+" (rerun)", again, wantCounters{})
			for i, p := range tc.probes {
				if n := outerRowCount(t, eng, p); n != tc.wantNum[i] {
					t.Fatalf("%s (rerun): %s returned %d rows, want %d", tc.clause, p, n, tc.wantNum[i])
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Non-empty drivers — unchanged
// ─────────────────────────────────────────────────────────────────────────────

// TestMergeNonEmptyDriver_FiresPerRow pins the controls the fix must not move: one
// row fires once, many rows fire many times (idempotently, because MERGE reads its
// own writes within the statement), and an OPTIONAL MATCH that matched nothing
// still emits ONE null-padded row — which IS a row, so the MERGE runs. That last
// case is the one a careless "suppress when the driver found nothing" fix breaks.
func TestMergeNonEmptyDriver_FiresPerRow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		setup  string
		prefix string
	}{
		{name: "one_row", setup: `CREATE (:M {name:'present'})`, prefix: `MATCH (m:M {name:'present'})`},
		{name: "two_rows", setup: `CREATE (:M {name:'x'}),(:M {name:'y'})`, prefix: `MATCH (m:M)`},
		{name: "optional_match_null_padded", prefix: `OPTIONAL MATCH (m:M {name:'absent'})`},
		{name: "unwind_one", prefix: `UNWIND [1] AS x`},
		{name: "unwind_three", prefix: `UNWIND [1,2,3] AS x`},
	} {
		for _, f := range []struct {
			name    string
			clause  string
			want    wantCounters
			probes  []string
			wantNum []int
		}{
			{
				name: "node", clause: zeroRowNodeMerge, want: zeroRowNodeEffect,
				probes:  []string{`MATCH (n:Q) RETURN n AS v`},
				wantNum: []int{1},
			},
			{
				name: "pattern", clause: zeroRowPatternMerge, want: zeroRowPatternEffect,
				probes:  []string{`MATCH (n:P) RETURN n AS v`, `MATCH ()-[r:T]->() RETURN r AS v`},
				wantNum: []int{2, 1},
			},
		} {
			t.Run(tc.name+"/"+f.name, func(t *testing.T) {
				t.Parallel()
				eng := setMapEng(t)
				if tc.setup != "" {
					drainRunInTx(t, eng, tc.setup)
				}
				stmt := tc.prefix + " " + f.clause
				c := runCountedP(t, eng, stmt, nil)
				assertCounters(t, stmt, c, f.want)
				for i, p := range f.probes {
					if n := outerRowCount(t, eng, p); n != f.wantNum[i] {
						t.Fatalf("%s: %s returned %d rows, want %d", stmt, p, n, f.wantNum[i])
					}
				}
			})
		}
	}
}

// TestMergeMixedDriver_PerBindingCardinality: one statement whose driver yields a
// row for one binding and none for another. The MERGE must run for the binding
// that produced a row and not for the one that did not — the per-row rule stated
// as a single statement rather than as two.
func TestMergeMixedDriver_PerBindingCardinality(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:M {name:'present'})`)
	const stmt = `UNWIND ['present','absent'] AS nm MATCH (m:M {name: nm}) MERGE (q:Q {name: nm})`
	c := runCountedP(t, eng, stmt, nil)
	assertCounters(t, stmt, c, zeroRowNodeEffect)
	if n := outerRowCount(t, eng, `MATCH (n:Q) RETURN n AS v`); n != 1 {
		t.Fatalf("%s: %d :Q nodes, want 1", stmt, n)
	}
	if n := outerRowCount(t, eng, `MATCH (n:Q {name:'present'}) RETURN n AS v`); n != 1 {
		t.Fatalf("%s: the binding that produced a row must have been merged, got %d", stmt, n)
	}
	if n := outerRowCount(t, eng, `MATCH (n:Q {name:'absent'}) RETURN n AS v`); n != 0 {
		t.Fatalf("%s: the binding that produced no row must not have been merged, got %d", stmt, n)
	}
}
