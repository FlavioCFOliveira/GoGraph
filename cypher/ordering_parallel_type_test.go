package cypher_test

// ordering_parallel_type_test.go — the parallel-edge relationship-TYPE oracle for
// the CSR within-source ordering (rmp #2143).
//
// This is the test the #2139 SPIKE identified as the highest-value missing guard,
// and it is deliberately NOT a differential against another build path: two prior
// false-greens in this project came from both arms of a differential sharing the
// broken code. Every expectation below is a count stated by hand from the fixture.
//
// What it guards: the HANDLE-EXACT resolution of a parallel edge's relationship
// type across the reordering. Ordering the run by (destination, handle) permutes
// which slot sits where, so a mis-permuted handle column silently assigns parallel
// edges each other's types; a per-destination count is how that surfaces.
//
// It does NOT reach buildEdgeTypeFilter's ORDINAL fallback, and an earlier draft of
// this file wrongly claimed it did on the premise that "a MERGE-created slot
// carries handle 0". That premise is false: MERGE creates through AddEdgeH
// (cypher/exec/merge_relationship.go, merge_pattern.go), so it mints a non-zero
// handle exactly as CREATE does. Measured on this fixture, every slot carries a
// non-zero handle, so the ordinal branch is never taken. Handle 0 arises only from
// a raw lpg.Graph.AddEdge or a store/txn OpAddEdge — never from Cypher — which is
// why TestOrdering_ParallelEdgeTypes_FixtureIsHandleBearing asserts the premise
// this file actually relies on instead of leaving it stated in a comment.
//
// The source's out-degree is deliberately past graph/csr's insertion-sort cutoff
// so the merge path is exercised, and the destinations are created in DESCENDING
// key order so the ordering has real work to do.
//
// Layer: short.

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// nDst is the destination count. 40 > graph/csr's insertion-sort cutoff of 32, so
// the source's run takes the stable-merge path rather than insertion sort.
const nDst = 40

// buildParallelTypeFixture creates one source with nDst destinations, each reached
// by THREE parallel relationships of distinct types. Destinations are created in
// descending key order so the CSR ordering must permute the run.
//
// Half the destinations get their third relationship via MERGE rather than CREATE.
// Both mint a handle, so this varies the WRITE PATH (merge-vs-create resolution)
// rather than the handle composition.
func buildParallelTypeFixture(t *testing.T) (*cypher.Engine, *lpg.Graph[string, float64]) {
	t.Helper()
	eng, g := inMemMultigraphEngine(t)
	mustRunWrite(t, eng, `CREATE (a:N {key:'src'})`)
	for d := nDst - 1; d >= 0; d-- { // descending
		key := fmt.Sprintf("d%02d", d)
		mustRunWrite(t, eng, fmt.Sprintf(`CREATE (b:N {key:'%s'})`, key))
		mustRunWrite(t, eng, fmt.Sprintf(
			`MATCH (a:N {key:'src'}),(b:N {key:'%s'}) CREATE (a)-[:T1 {n:%d}]->(b)`, key, d))
		mustRunWrite(t, eng, fmt.Sprintf(
			`MATCH (a:N {key:'src'}),(b:N {key:'%s'}) CREATE (a)-[:T2 {n:%d}]->(b)`, key, d))
		if d%2 == 0 {
			// MERGE-created: handle 0, so buildEdgeTypeFilter takes the ordinal path.
			mustRunWrite(t, eng, fmt.Sprintf(
				`MATCH (a:N {key:'src'}),(b:N {key:'%s'}) MERGE (a)-[:T3]->(b)`, key))
		} else {
			mustRunWrite(t, eng, fmt.Sprintf(
				`MATCH (a:N {key:'src'}),(b:N {key:'%s'}) CREATE (a)-[:T3 {n:%d}]->(b)`, key, d))
		}
	}
	return eng, g
}

// TestOrdering_ParallelEdgeTypes_AbsoluteOracle states every expected count by
// hand. A mis-typed parallel edge moves at least one of them.
func TestOrdering_ParallelEdgeTypes_AbsoluteOracle(t *testing.T) {
	t.Parallel()
	eng, _ := buildParallelTypeFixture(t)

	// Hand-computed: nDst destinations x 3 parallel relationships.
	if got := countScalar(t, eng, `MATCH (:N {key:'src'})-[r]->() RETURN count(r) AS c`); got != nDst*3 {
		t.Errorf("total relationships from src = %d, want %d", got, nDst*3)
	}

	// Hand-computed: exactly one relationship of each type per destination.
	for _, typ := range []string{"T1", "T2", "T3"} {
		q := fmt.Sprintf(`MATCH (:N {key:'src'})-[r:%s]->() RETURN count(r) AS c`, typ)
		if got := countScalar(t, eng, q); got != nDst {
			t.Errorf("relationships of type %s = %d, want %d", typ, got, nDst)
		}
	}

	// Per destination, each type must appear exactly once — this is the assertion a
	// mis-assigned ordinal breaks, because it moves a type from one parallel slot
	// to another WITHOUT changing the totals above.
	for d := 0; d < nDst; d++ {
		key := fmt.Sprintf("d%02d", d)
		for _, typ := range []string{"T1", "T2", "T3"} {
			q := fmt.Sprintf(
				`MATCH (:N {key:'src'})-[r:%s]->(b:N {key:'%s'}) RETURN count(r) AS c`, typ, key)
			if got := countScalar(t, eng, q); got != 1 {
				t.Errorf("type %s to %s = %d, want exactly 1", typ, key, got)
			}
		}
	}
}

// TestOrdering_ParallelEdgeTypes_MultiTypeFilter exercises the multi-type
// [r:A|B] admission set over the same fixture, which is a different path through
// buildEdgeTypeFilter than a single type.
func TestOrdering_ParallelEdgeTypes_MultiTypeFilter(t *testing.T) {
	t.Parallel()
	eng, _ := buildParallelTypeFixture(t)

	if got := countScalar(t, eng, `MATCH (:N {key:'src'})-[r:T1|T2]->() RETURN count(r) AS c`); got != nDst*2 {
		t.Errorf("relationships of type T1|T2 = %d, want %d", got, nDst*2)
	}
	if got := countScalar(t, eng, `MATCH (:N {key:'src'})-[r:T1|T2|T3]->() RETURN count(r) AS c`); got != nDst*3 {
		t.Errorf("relationships of type T1|T2|T3 = %d, want %d", got, nDst*3)
	}
	// An absent type must admit nothing, which pins that the filter is not simply
	// permissive after the reorder.
	if got := countScalar(t, eng, `MATCH (:N {key:'src'})-[r:NOPE]->() RETURN count(r) AS c`); got != 0 {
		t.Errorf("relationships of an absent type = %d, want 0", got)
	}
}

// The REVERSE direction is deliberately NOT asserted here. Doing so uncovered a
// PRE-EXISTING engine defect, reproduced identically on the pre-#2141 code and so
// unrelated to the ordering work: with two or more parallel relationships of
// DISTINCT types between one ordered pair, a reverse-direction type-filtered match
// admits every parallel edge as the FIRST type and matches nothing for the others.
// It is filed as its own BUG task with the reproduction; the reverse assertions
// belong with the fix, not here, because a test that fails for an unrelated reason
// would mask a regression in this one.

// TestOrdering_ParallelEdgeTypes_FixtureIsHandleBearing asserts the fixture's own
// premise rather than trusting a comment. If a future change made any Cypher write
// path leave handle 0, the oracle above would silently start covering a different
// (ordinal) resolution path, and this test says so.
func TestOrdering_ParallelEdgeTypes_FixtureIsHandleBearing(t *testing.T) {
	t.Parallel()
	_, g := buildParallelTypeFixture(t)

	handles := csr.BuildFromAdjList(g.AdjList()).HandlesSlice()
	if handles == nil {
		t.Fatal("fixture CSR carries no handle column at all")
	}
	zeros := 0
	for _, h := range handles {
		if h == 0 {
			zeros++
		}
	}
	if zeros != 0 {
		t.Errorf("fixture has %d handle-0 slots out of %d; every Cypher write path is "+
			"expected to mint a handle, so the oracle covers handle-EXACT resolution. "+
			"If this now holds, the oracle's coverage has changed and its header is stale",
			zeros, len(handles))
	}
}

// TestOrdering_QueryResultsMatchAdjacencyOracle is the query-level differential the
// ordering work owes: emitted results must agree with an oracle read straight from
// the ADJACENCY, which the ordering never touches.
//
// It is written against the adjacency rather than against an "ordering disabled"
// build because #2141 made the ordering unconditional — there is no unordered CSR
// to compare with in production. The adjacency is the better reference anyway: it
// is a genuinely independent source rather than the same builder with a flag
// flipped, so it cannot go green over a defect both arms share.
func TestOrdering_QueryResultsMatchAdjacencyOracle(t *testing.T) {
	t.Parallel()
	eng, g := inMemMultigraphEngine(t)
	mustRunWrite(t, eng, `CREATE (a:N {key:'src'})`)
	// Descending destinations past the insertion-sort cutoff, with parallel edges on
	// every third destination so runs of length > 1 exist.
	want := 0
	for d := 49; d >= 0; d-- {
		key := fmt.Sprintf("d%02d", d)
		mustRunWrite(t, eng, fmt.Sprintf(`CREATE (b:N {key:'%s'})`, key))
		reps := 1
		if d%3 == 0 {
			reps = 3
		}
		for r := 0; r < reps; r++ {
			mustRunWrite(t, eng, fmt.Sprintf(
				`MATCH (a:N {key:'src'}),(b:N {key:'%s'}) CREATE (a)-[:R]->(b)`, key))
			want++
		}
	}

	// Oracle: the adjacency's own out-degree for the source, read without any CSR.
	srcKey := keyNode(t, g, "src")
	adjDegree, ok := g.AdjList().OutDegree(srcKey)
	if !ok {
		t.Fatal("source absent from the adjacency")
	}
	if adjDegree != want {
		t.Fatalf("fixture broken: adjacency out-degree %d, want %d", adjDegree, want)
	}

	// The query traverses the ORDERED CSR; the count must match the adjacency.
	if got := countScalar(t, eng,
		`MATCH (:N {key:'src'})-[r:R]->() RETURN count(r) AS c`); got != int64(want) {
		t.Errorf("query over the ordered CSR returned %d relationships, adjacency has %d", got, want)
	}
	// Per destination, the emitted count must match that pair's slot count.
	for d := 0; d < 50; d++ {
		expected := int64(1)
		if d%3 == 0 {
			expected = 3
		}
		q := fmt.Sprintf(
			`MATCH (:N {key:'src'})-[r:R]->(b:N {key:'d%02d'}) RETURN count(r) AS c`, d)
		if got := countScalar(t, eng, q); got != expected {
			t.Errorf("d%02d: query returned %d, adjacency has %d slots", d, got, expected)
		}
	}
	// And the reverse direction must agree on the total, which routes through the
	// reverse CSR and the reverse-to-forward mapping.
	if got := countScalar(t, eng,
		`MATCH (b:N)<-[r:R]-(:N {key:'src'}) RETURN count(r) AS c`); got != int64(want) {
		t.Errorf("reverse traversal returned %d relationships, adjacency has %d", got, want)
	}
}
