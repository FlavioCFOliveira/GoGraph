package cypher

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/count"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newCountEngine builds a directed multigraph engine (the openCypher storage
// model) with the given per-relabel OUT recount budget (0 → default).
func newCountEngine(t *testing.T, budget int) (*Engine, *lpg.Graph[string, float64]) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := NewEngineWithOptions(g, EngineOptions{MaxLabelRecountEdges: budget})
	return eng, g
}

func mustRun(t *testing.T, eng *Engine, q string) {
	t.Helper()
	r, err := eng.RunInTx(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("RunInTx(%q): %v", q, err)
	}
	// Drain so any lazy work completes; write results are materialised already.
	for r.Next() {
	}
	if err := r.Err(); err != nil {
		t.Fatalf("RunInTx(%q) drain: %v", q, err)
	}
	_ = r.Close()
}

// oracleLabelIDs reads a node's interned label ids independently of the
// maintenance path (via NodeLabelsByID rather than ForEachNodeLabelByID).
func oracleLabelIDs(g *lpg.Graph[string, float64], id graph.NodeID) []uint32 {
	reg := g.Registry()
	names := g.NodeLabelsByID(id)
	out := make([]uint32, 0, len(names))
	for _, name := range names {
		if lid, ok := reg.Lookup(name); ok {
			out = append(out, uint32(lid))
		}
	}
	return out
}

// recount computes the exact E/D/T counts by a full O(V+E) pass over the live
// graph — the ground-truth oracle the incrementally-maintained store must match.
func recount(g *lpg.Graph[string, float64]) count.Snapshot {
	exp := count.Snapshot{
		E:    map[uint32]int64{},
		DOut: map[uint64]int64{},
		DIn:  map[uint64]int64{},
		T:    map[[3]uint32]int64{},
	}
	g.AdjList().Mapper().Walk(func(srcID graph.NodeID, _ string) bool {
		if g.IsTombstoned(srcID) {
			return true
		}
		nbs, _, handles := g.AdjList().LoadEntryH(srcID)
		labs := g.AdjList().LoadEntryLabels(srcID)
		srcLabels := oracleLabelIDs(g, srcID)
		for i, dstID := range nbs {
			if g.IsTombstoned(dstID) {
				continue
			}
			dstLabels := oracleLabelIDs(g, dstID)
			forEachSlotRelType(g, srcID, dstID, handles, labs, i, func(rt uint32) {
				exp.E[rt]++
				for _, la := range srcLabels {
					exp.DOut[uint64(la)<<32|uint64(rt)]++
				}
				for _, lb := range dstLabels {
					exp.DIn[uint64(lb)<<32|uint64(rt)]++
				}
				for _, la := range srcLabels {
					for _, lb := range dstLabels {
						exp.T[[3]uint32{la, rt, lb}]++
					}
				}
			})
		}
		return true
	})
	return exp
}

func setOf(ids []uint32) map[uint32]bool {
	m := make(map[uint32]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// diffCounts compares the store against a fresh O(V+E) recount and returns one
// message per mismatch (an empty slice means they agree). It skips only the cells
// the store has flagged dirty (E is compared unconditionally — it is never
// dirty). The comparison is bidirectional: it catches both under-counts (a
// missing/low store cell) and over-counts or leaks (a stray store cell the
// recount does not produce). Returning messages rather than calling t.Errorf lets
// both the *testing.T assertion wrapper and the rapid property test (whose T has
// no Helper) reuse the exact comparison and report through their own failers.
func diffCounts(cs *count.Store, g *lpg.Graph[string, float64]) []string {
	exp := recount(g)
	got := cs.Snapshot()
	dOut, dIn := setOf(got.DirtyDOut), setOf(got.DirtyDIn)
	tA, tB := setOf(got.DirtyTA), setOf(got.DirtyTB)

	var diffs []string
	add := func(format string, args ...any) { diffs = append(diffs, fmt.Sprintf(format, args...)) }

	// E: exact both ways, always.
	for rt, v := range exp.E {
		if got.E[rt] != v {
			add("E(%d) store=%d oracle=%d", rt, got.E[rt], v)
		}
	}
	for rt, v := range got.E {
		if exp.E[rt] != v {
			add("E(%d) store=%d (leak) oracle=%d", rt, v, exp.E[rt])
		}
	}

	cmpD := func(name string, expM, gotM map[uint64]int64, dirty map[uint32]bool) {
		for k, v := range expM {
			if dirty[uint32(k>>32)] {
				continue
			}
			if gotM[k] != v {
				add("%s[label=%d,rt=%d] store=%d oracle=%d", name, uint32(k>>32), uint32(k), gotM[k], v)
			}
		}
		for k, v := range gotM {
			if dirty[uint32(k>>32)] {
				continue
			}
			if expM[k] != v {
				add("%s[label=%d,rt=%d] store=%d (leak) oracle=%d", name, uint32(k>>32), uint32(k), v, expM[k])
			}
		}
	}
	cmpD("DOut", exp.DOut, got.DOut, dOut)
	cmpD("DIn", exp.DIn, got.DIn, dIn)

	tDirty := func(k [3]uint32) bool { return tA[k[0]] || tB[k[2]] }
	for k, v := range exp.T {
		if tDirty(k) {
			continue
		}
		if got.T[k] != v {
			add("T%v store=%d oracle=%d", k, got.T[k], v)
		}
	}
	for k, v := range got.T {
		if tDirty(k) {
			continue
		}
		if exp.T[k] != v {
			add("T%v store=%d (leak) oracle=%d", k, v, exp.T[k])
		}
	}
	return diffs
}

// assertCountsMatch verifies the store equals the oracle on every non-dirty cell,
// failing t with one error per mismatch. It is the *testing.T wrapper over
// [diffCounts].
func assertCountsMatch(t *testing.T, cs *count.Store, g *lpg.Graph[string, float64]) {
	t.Helper()
	for _, d := range diffCounts(cs, g) {
		t.Errorf("%s", d)
	}
}

// TestCountStore_PureCreateIsCleanAndExact confirms that a graph built purely by
// CREATE never dirties any D/T family — because CREATE assigns node labels before
// any edge exists (Size()==0 fast path) — and that every cell matches the recount.
func TestCountStore_PureCreateIsCleanAndExact(t *testing.T) {
	eng, g := newCountEngine(t, 0)
	mustRun(t, eng, "CREATE (:A)")                                          // bare labelled node
	mustRun(t, eng, "CREATE (:A)-[:R]->(:B)")                               // simple typed edge
	mustRun(t, eng, "CREATE (:A:C)-[:R]->(:B)")                             // multi-label source
	mustRun(t, eng, "CREATE (:A)-[:S]->(:B:D)")                             // multi-label dest
	mustRun(t, eng, "CREATE (:A)-[:R]->(:B)-[:S]->(:C)")                    // chain
	mustRun(t, eng, "MATCH (a:A),(b:B) CREATE (a)-[:R]->(b),(a)-[:S]->(b)") // parallel edges, diff types

	snap := cs(eng).Snapshot()
	if len(snap.DirtyDOut)+len(snap.DirtyDIn)+len(snap.DirtyTA)+len(snap.DirtyTB) != 0 {
		t.Fatalf("pure CREATE dirtied D/T: %+v", snap)
	}
	assertCountsMatch(t, cs(eng), g)

	// Independent E cross-check: every edge is typed, so sum(E) == live edge count.
	var sumE int64
	for _, v := range snap.E {
		sumE += v
	}
	if uint64(sumE) != g.AdjList().Size() {
		t.Fatalf("sum(E)=%d != Size()=%d", sumE, g.AdjList().Size())
	}
}

// cs returns an engine's count store.
func cs(e *Engine) *count.Store { return e.countStore }

// TestCountStore_DeleteDecrements confirms edge deletion frees the cells.
func TestCountStore_DeleteDecrements(t *testing.T) {
	eng, g := newCountEngine(t, 0)
	mustRun(t, eng, "CREATE (:A)-[:R]->(:B)")
	mustRun(t, eng, "CREATE (:A)-[:R]->(:B)")
	assertCountsMatch(t, cs(eng), g)
	if cs(eng).CountE(idOf(t, g, "R")) != 2 {
		t.Fatalf("E(R) want 2")
	}
	mustRun(t, eng, "MATCH (:A)-[r:R]->(:B) DELETE r")
	assertCountsMatch(t, cs(eng), g)
	if got := cs(eng).CountE(idOf(t, g, "R")); got != 0 {
		t.Fatalf("E(R) after DELETE all = %d, want 0", got)
	}
}

// TestCountStore_DetachDeleteDecrements confirms DETACH DELETE removes a node's
// incident-edge counts (out-edges via RemoveAllEdgesFrom, in-edges via RemoveEdge).
func TestCountStore_DetachDeleteDecrements(t *testing.T) {
	eng, g := newCountEngine(t, 0)
	mustRun(t, eng, "CREATE (:A)-[:R]->(:B)-[:S]->(:C)")
	// Delete the middle node: strips its out-edge (S) and in-edge (R).
	mustRun(t, eng, "MATCH (b:B) DETACH DELETE b")
	assertCountsMatch(t, cs(eng), g)
	if got := cs(eng).CountE(idOf(t, g, "R")); got != 0 {
		t.Fatalf("E(R) after DETACH DELETE b = %d, want 0", got)
	}
	if got := cs(eng).CountE(idOf(t, g, "S")); got != 0 {
		t.Fatalf("E(S) after DETACH DELETE b = %d, want 0", got)
	}
}

func idOf(t *testing.T, g *lpg.Graph[string, float64], name string) uint32 {
	t.Helper()
	id, ok := g.Registry().Lookup(name)
	if !ok {
		t.Fatalf("label/type %q not interned", name)
	}
	return uint32(id)
}

// TestCountStore_RelabelDirtyScoping verifies the OUT-exact / IN-dirty X-scoping
// of design §3.3.1 for a SET on a source node.
func TestCountStore_RelabelDirtyScoping(t *testing.T) {
	eng, g := newCountEngine(t, 0)
	mustRun(t, eng, "CREATE (:A)-[:R]->(:B)")
	// a gains label X. a is the source of the R edge (has an out-edge).
	mustRun(t, eng, "MATCH (a:A) SET a:X")

	snap := cs(eng).Snapshot()
	x := idOf(t, g, "X")
	r := idOf(t, g, "R")
	b := idOf(t, g, "B")

	// OUT side is exact: D(X,R,OUT)=1 and T(X,R,B)=1, and NOT dirty.
	if setOf(snap.DirtyDOut)[x] {
		t.Errorf("D(X,*,OUT) must stay exact after a source relabel")
	}
	if setOf(snap.DirtyTA)[x] {
		t.Errorf("T(X,*,*) must stay exact after a source relabel")
	}
	if got := cs(eng).CountD(x, r, count.Out); got != 1 {
		t.Errorf("D(X,R,OUT)=%d, want 1", got)
	}
	if got := cs(eng).CountT(x, r, b); got != 1 {
		t.Errorf("T(X,R,B)=%d, want 1", got)
	}
	// IN side is dirty (X-scoped): D(X,*,IN) and T(*,*,X).
	if !setOf(snap.DirtyDIn)[x] {
		t.Errorf("D(X,*,IN) must be dirty after a relabel")
	}
	if !setOf(snap.DirtyTB)[x] {
		t.Errorf("T(*,*,X) must be dirty after a relabel")
	}
	// The pre-existing exact cells are untouched.
	assertCountsMatch(t, cs(eng), g)
}

// TestCountStore_BudgetTripDirtiesOut sets a tiny budget and relabels a high
// out-degree node, tripping the OUT recount ceiling into an X-scoped OUT dirty.
func TestCountStore_BudgetTripDirtiesOut(t *testing.T) {
	eng, g := newCountEngine(t, 2) // budget = 2 out-edges
	mustRun(t, eng, "CREATE (h:A)")
	// Give h four out-edges (over the budget of 2).
	mustRun(t, eng, "MATCH (h:A) CREATE (h)-[:R]->(:B),(h)-[:R]->(:B),(h)-[:R]->(:B),(h)-[:R]->(:B)")
	mustRun(t, eng, "MATCH (h:A) SET h:X")

	snap := cs(eng).Snapshot()
	x := idOf(t, g, "X")
	if !setOf(snap.DirtyDOut)[x] {
		t.Errorf("over-budget relabel must dirty D(X,*,OUT)")
	}
	if !setOf(snap.DirtyTA)[x] {
		t.Errorf("over-budget relabel must dirty T(X,*,*)")
	}
	// E is never dirty and must still match the recount.
	if got := cs(eng).CountE(idOf(t, g, "R")); got != 4 {
		t.Errorf("E(R)=%d, want 4", got)
	}
	assertCountsMatch(t, cs(eng), g)
}

// TestCountStore_MixedWorkloadMatchesRecount runs a deterministic pseudo-random
// mix of CREATE / DELETE / SET-label / REMOVE-label and checks the store against
// the ground-truth recount after every batch, plus one healing RecomputeReset +
// recompute at the end that must reconcile every cell exactly.
func TestCountStore_MixedWorkloadMatchesRecount(t *testing.T) {
	eng, g := newCountEngine(t, 8)
	rng := rand.New(rand.NewSource(20260723))
	labels := []string{"A", "B", "C", "D"}
	types := []string{"R", "S", "T"}

	for step := 0; step < 400; step++ {
		switch rng.Intn(5) {
		case 0, 1: // CREATE a typed edge between two fresh labelled nodes
			la := labels[rng.Intn(len(labels))]
			lb := labels[rng.Intn(len(labels))]
			rt := types[rng.Intn(len(types))]
			mustRun(t, eng, "CREATE (:"+la+")-[:"+rt+"]->(:"+lb+")")
		case 2: // relabel: add a label to some node of a known label
			from := labels[rng.Intn(len(labels))]
			to := labels[rng.Intn(len(labels))]
			// SET is a no-op-safe operation even if no node matches.
			mustRun(t, eng, "MATCH (n:"+from+") WITH n LIMIT 1 SET n:"+to)
		case 3: // remove a label
			l := labels[rng.Intn(len(labels))]
			mustRun(t, eng, "MATCH (n:"+l+") WITH n LIMIT 1 REMOVE n:"+l)
		case 4: // delete one relationship of a random type
			rt := types[rng.Intn(len(types))]
			mustRun(t, eng, "MATCH ()-[r:"+rt+"]->() WITH r LIMIT 1 DELETE r")
		}
		if step%25 == 0 {
			assertCountsMatch(t, cs(eng), g)
		}
	}
	assertCountsMatch(t, cs(eng), g)
}
