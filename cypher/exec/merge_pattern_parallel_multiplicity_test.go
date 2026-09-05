package exec_test

// merge_pattern_parallel_multiplicity_test.go — regression for rmp #1875.
//
// A MERGE whose relationship endpoints are BOTH bound and whose ON CREATE /
// ON MATCH action targets a NODE variable routes through the general
// MergePattern operator (not the MergeRelationship fast path). MergePattern's
// search under-counted relationship multiplicity for pre-existing PARALLEL
// edges: a hop between a resolved node pair with two parallel `:T` edges
// produced a single joint binding, so
//
//	MATCH (a:A),(b:B) MERGE (a)-[r:T]->(b) ON CREATE SET a.x = 1 RETURN count(r)
//
// returned 1 while the equivalent MATCH (a:A)-[r:T]->(b:B) RETURN count(r)
// returned 2. The fix fans out one binding per pre-existing parallel
// relationship satisfying the hop's type/property predicate, so MERGE's match
// multiplicity equals the MATCH multiplicity, without altering the
// match-vs-create decision or creating any duplicate node/edge.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newMultigraphEngine builds a directed multigraph engine — the openCypher
// storage model that keeps parallel relationships as distinct instances.
func newMultigraphEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	return cypher.NewEngine(g)
}

// execWrite runs a write query to completion and fails the test on error.
func execWrite(t *testing.T, eng *cypher.Engine, query string) {
	t.Helper()
	res, err := eng.RunInTxAny(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("write %q: %v", query, err)
	}
	for res.Next() { // drain
	}
	if cerr := res.Err(); cerr != nil {
		t.Fatalf("write %q result error: %v", query, cerr)
	}
	_ = res.Close()
}

// runScalarCount runs a `RETURN count(...) AS c` query and returns c.
func runScalarCount(t *testing.T, eng *cypher.Engine, query string) int64 {
	t.Helper()
	res, err := eng.RunInTxAny(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	defer res.Close() // test cleanup
	var got int64
	var rows int
	for res.Next() {
		rows++
		rec := res.Record()
		v, ok := rec["c"].(expr.IntegerValue)
		if !ok {
			t.Fatalf("count query %q: c is %T (%v), want IntegerValue", query, rec["c"], rec["c"])
		}
		got = int64(v)
	}
	if cerr := res.Err(); cerr != nil {
		t.Fatalf("count query %q result error: %v", query, cerr)
	}
	if rows != 1 {
		t.Fatalf("count query %q: got %d rows, want 1", query, rows)
	}
	return got
}

// runScalarProp runs a query returning `RETURN <expr> AS v` for the single
// matched row and reports the value and whether exactly one row was produced.
func runScalarProp(t *testing.T, eng *cypher.Engine, query string) (expr.Value, bool) {
	t.Helper()
	res, err := eng.RunInTxAny(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("scalar query %q: %v", query, err)
	}
	defer res.Close() // test cleanup
	var v expr.Value
	var rows int
	for res.Next() {
		rows++
		if val, isVal := res.Record()["v"].(expr.Value); isVal {
			v = val
		}
	}
	if cerr := res.Err(); cerr != nil {
		t.Fatalf("scalar query %q result error: %v", query, cerr)
	}
	return v, rows == 1
}

// TestMergePattern_ParallelEdgeMultiplicity_TwoParallel is the #1875 repro.
func TestMergePattern_ParallelEdgeMultiplicity_TwoParallel(t *testing.T) {
	t.Parallel()
	eng := newMultigraphEngine(t)

	execWrite(t, eng, `CREATE (a:A), (b:B)`)
	execWrite(t, eng, `MATCH (a:A),(b:B) CREATE (a)-[:T]->(b)`)
	execWrite(t, eng, `MATCH (a:A),(b:B) CREATE (a)-[:T]->(b)`)

	// Baseline: a plain MATCH sees two parallel relationships.
	if got := runScalarCount(t, eng,
		`MATCH (a:A)-[r:T]->(b:B) RETURN count(r) AS c`); got != 2 {
		t.Fatalf("baseline MATCH count(r) = %d, want 2", got)
	}

	// The MERGE (both endpoints bound; ON CREATE targets a node var → routes
	// through MergePattern) must report the same multiplicity as the MATCH.
	if got := runScalarCount(t, eng,
		`MATCH (a:A),(b:B) MERGE (a)-[r:T]->(b) ON CREATE SET a.x = 1 RETURN count(r) AS c`); got != 2 {
		t.Fatalf("MERGE count(r) = %d, want 2 (must equal MATCH count)", got)
	}

	// The MERGE matched (did not create): no third edge, and ON CREATE did not
	// fire, so a.x is unset.
	if got := runScalarCount(t, eng,
		`MATCH (a:A)-[r:T]->(b:B) RETURN count(r) AS c`); got != 2 {
		t.Fatalf("post-MERGE MATCH count(r) = %d, want 2 (MERGE must not create a duplicate edge)", got)
	}
	if v, ok := runScalarProp(t, eng, `MATCH (a:A) RETURN a.x AS v`); !ok || !expr.IsNull(v) {
		t.Fatalf("a.x = %v (ok=%v), want null (ON CREATE must not fire on a match)", v, ok)
	}
	// Exactly one :A node exists (no duplicate node created).
	if got := runScalarCount(t, eng, `MATCH (a:A) RETURN count(a) AS c`); got != 1 {
		t.Fatalf(":A node count = %d, want 1 (no duplicate node)", got)
	}
}

// TestMergePattern_ParallelEdgeMultiplicity_Triple covers three parallel edges.
func TestMergePattern_ParallelEdgeMultiplicity_Triple(t *testing.T) {
	t.Parallel()
	eng := newMultigraphEngine(t)

	execWrite(t, eng, `CREATE (a:A), (b:B)`)
	execWrite(t, eng, `MATCH (a:A),(b:B) CREATE (a)-[:T]->(b)`)
	execWrite(t, eng, `MATCH (a:A),(b:B) CREATE (a)-[:T]->(b)`)
	execWrite(t, eng, `MATCH (a:A),(b:B) CREATE (a)-[:T]->(b)`)

	if got := runScalarCount(t, eng,
		`MATCH (a:A)-[r:T]->(b:B) RETURN count(r) AS c`); got != 3 {
		t.Fatalf("baseline MATCH count(r) = %d, want 3", got)
	}
	if got := runScalarCount(t, eng,
		`MATCH (a:A),(b:B) MERGE (a)-[r:T]->(b) ON CREATE SET a.x = 1 RETURN count(r) AS c`); got != 3 {
		t.Fatalf("MERGE count(r) = %d, want 3", got)
	}
	if got := runScalarCount(t, eng,
		`MATCH (a:A)-[r:T]->(b:B) RETURN count(r) AS c`); got != 3 {
		t.Fatalf("post-MERGE MATCH count(r) = %d, want 3 (no duplicate edge)", got)
	}
}

// TestMergePattern_ParallelEdgeMultiplicity_MixedTypesFiltered proves the
// fan-out counts only the parallel edges whose type satisfies the hop
// predicate: with two :T and one :X parallel edge, MERGE (a)-[:T]->(b) matches
// the same 2 the equivalent MATCH does — not all 3.
func TestMergePattern_ParallelEdgeMultiplicity_MixedTypesFiltered(t *testing.T) {
	t.Parallel()
	eng := newMultigraphEngine(t)

	execWrite(t, eng, `CREATE (a:A), (b:B)`)
	execWrite(t, eng, `MATCH (a:A),(b:B) CREATE (a)-[:T]->(b)`)
	execWrite(t, eng, `MATCH (a:A),(b:B) CREATE (a)-[:T]->(b)`)
	execWrite(t, eng, `MATCH (a:A),(b:B) CREATE (a)-[:X]->(b)`)

	if got := runScalarCount(t, eng,
		`MATCH (a:A)-[r:T]->(b:B) RETURN count(r) AS c`); got != 2 {
		t.Fatalf("baseline MATCH count(r:T) = %d, want 2", got)
	}
	if got := runScalarCount(t, eng,
		`MATCH (a:A),(b:B) MERGE (a)-[r:T]->(b) ON CREATE SET a.x = 1 RETURN count(r) AS c`); got != 2 {
		t.Fatalf("MERGE count(r:T) = %d, want 2 (type filter must exclude the :X edge)", got)
	}
}

// TestMergePattern_MergeCreatesWhenNoneMatch confirms the match-vs-create
// decision is unchanged: with no pre-existing edge the MERGE creates exactly
// one, ON CREATE fires, and count(r) is 1.
func TestMergePattern_MergeCreatesWhenNoneMatch(t *testing.T) {
	t.Parallel()
	eng := newMultigraphEngine(t)

	execWrite(t, eng, `CREATE (a:A), (b:B)`)

	if got := runScalarCount(t, eng,
		`MATCH (a:A),(b:B) MERGE (a)-[r:T]->(b) ON CREATE SET a.x = 1 RETURN count(r) AS c`); got != 1 {
		t.Fatalf("MERGE (no pre-existing edge) count(r) = %d, want 1", got)
	}
	// Exactly one edge was created.
	if got := runScalarCount(t, eng,
		`MATCH (a:A)-[r:T]->(b:B) RETURN count(r) AS c`); got != 1 {
		t.Fatalf("post-MERGE MATCH count(r) = %d, want 1 (exactly one edge created)", got)
	}
	// ON CREATE fired: a.x == 1.
	v, ok := runScalarProp(t, eng, `MATCH (a:A) RETURN a.x AS v`)
	if !ok {
		t.Fatal("expected exactly one :A node")
	}
	iv, isInt := v.(expr.IntegerValue)
	if !isInt || int64(iv) != 1 {
		t.Fatalf("a.x = %v (%T), want IntegerValue(1) (ON CREATE must fire on create)", v, v)
	}
}
