package search

import (
	"context"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// acquireDijkHeap has three call sites — DijkstraInto, AStarInto and
// PrimMSTCtx — and rmp #2862 changed the accessor itself, so all three
// move together. TestDijkstraInto_HeapBackingPreSized pins the first.
// This file pins the other two: one test per call site, each asserting
// both halves of the claim — that the cold-pool allocation count drops,
// and that the result is unchanged.
//
// The pattern is the same as the Dijkstra test: two runtime.GC() calls
// empty the pool (primary to victim, then victim dropped), so every
// iteration acquires a genuinely fresh heap — the state a high allocation
// rate keeps the pool in.

// astarIntoMallocCeiling and primMSTMallocCeiling are ceilings over a
// FIXED workload (a dijkstraIntoStarSpan-spoke star, whose priority-queue
// high-water mark is the spoke count).
//
// Measured on this shape, go1.27.1 darwin/arm64 without -race:
//
//	AStarInto  : 963 heap objects at 8affe124, 222 after the fix
//	PrimMST    : 1157 heap objects at 8affe124, 405 after the fix
//
// PrimMST's floor is higher because it allocates its four result and
// scratch arrays per call regardless; only the heap backing is at issue
// here. Each ceiling sits between the two measurements.
const (
	astarIntoMallocCeiling = 400
	primMSTMallocCeiling   = 700
)

// TestAStarInto_HeapBackingPreSized pins the AStarInto call site of
// acquireDijkHeap: the cold-pool allocation count, and the identity of
// the path and cost it returns.
func TestAStarInto_HeapBackingPreSized(t *testing.T) {
	c := denseStarCSR(t, dijkstraIntoStarSpan)
	maxID := uint64(c.MaxNodeID())
	dist := make([]int64, maxID)
	parent := make([]graph.NodeID, maxID)
	found := make([]bool, maxID)
	var path []graph.NodeID
	ctx := context.Background()
	// A zero heuristic makes A* behave as Dijkstra, so the expected path
	// and cost are the star's single edge and its weight.
	zeroH := func(graph.NodeID) int64 { return 0 }
	const dst = 1

	// Warm pool: the reference result and the one-off process set-up.
	wantCost, err := AStarInto(ctx, c, 0, dst, zeroH, dist, parent, found, &path)
	if err != nil {
		t.Fatalf("warm-up AStarInto: %v", err)
	}
	wantPath := append([]graph.NodeID(nil), path...)
	if len(wantPath) != 2 || wantPath[0] != 0 || wantPath[1] != dst {
		t.Fatalf("warm-up path = %v, want [0 %d]", wantPath, dst)
	}
	if wantCost != int64(dijkstraIntoStarSpan-dst) {
		t.Fatalf("warm-up cost = %d, want %d", wantCost, dijkstraIntoStarSpan-dst)
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	for i := 0; i < dijkstraIntoColdPoolRounds; i++ {
		runtime.GC()
		runtime.GC()
		cost, err := AStarInto(ctx, c, 0, dst, zeroH, dist, parent, found, &path)
		if err != nil {
			t.Fatalf("AStarInto round %d: %v", i, err)
		}
		// Result identity across a cold pool: the heap hint must change
		// nothing the caller can see.
		if cost != wantCost {
			t.Fatalf("round %d: cost = %d, want %d", i, cost, wantCost)
		}
		if len(path) != len(wantPath) {
			t.Fatalf("round %d: path = %v, want %v", i, path, wantPath)
		}
		for k := range wantPath {
			if path[k] != wantPath[k] {
				t.Fatalf("round %d: path = %v, want %v", i, path, wantPath)
			}
		}
	}

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	mallocs := after.Mallocs - before.Mallocs
	if mallocs > astarIntoMallocCeiling {
		t.Fatalf("%d cold-pool AStarInto calls allocated %d heap objects, ceiling %d: the heap backing is being grown by append (rmp #2862)",
			dijkstraIntoColdPoolRounds, mallocs, astarIntoMallocCeiling)
	}
	t.Logf("%d cold-pool AStarInto calls: %d heap objects (ceiling %d)",
		dijkstraIntoColdPoolRounds, mallocs, astarIntoMallocCeiling)
}

// TestPrimMST_HeapBackingPreSized pins the PrimMSTCtx call site of
// acquireDijkHeap: the cold-pool allocation count, and the identity of
// the spanning tree and total weight it returns.
func TestPrimMST_HeapBackingPreSized(t *testing.T) {
	c := denseStarCSR(t, dijkstraIntoStarSpan)

	wantParent, wantFound, wantTotal, err := PrimMST(c, 0)
	if err != nil {
		t.Fatalf("warm-up PrimMST: %v", err)
	}
	// The star's MST is the star itself: every spoke attaches to node 0.
	var expectTotal int64
	for v := 1; v < dijkstraIntoStarSpan; v++ {
		if !wantFound[v] || wantParent[v] != 0 {
			t.Fatalf("warm-up spoke %d: found=%t parent=%d, want true and 0", v, wantFound[v], wantParent[v])
		}
		expectTotal += int64(dijkstraIntoStarSpan - v)
	}
	if wantTotal != expectTotal {
		t.Fatalf("warm-up total weight = %d, want %d", wantTotal, expectTotal)
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	for i := 0; i < dijkstraIntoColdPoolRounds; i++ {
		runtime.GC()
		runtime.GC()
		parent, found, total, err := PrimMST(c, 0)
		if err != nil {
			t.Fatalf("PrimMST round %d: %v", i, err)
		}
		if total != wantTotal {
			t.Fatalf("round %d: total weight = %d, want %d", i, total, wantTotal)
		}
		if len(parent) != len(wantParent) || len(found) != len(wantFound) {
			t.Fatalf("round %d: result lengths %d/%d, want %d/%d",
				i, len(parent), len(found), len(wantParent), len(wantFound))
		}
		for v := range wantParent {
			if found[v] != wantFound[v] || (found[v] && parent[v] != wantParent[v]) {
				t.Fatalf("round %d: node %d = (%d, %t), want (%d, %t)",
					i, v, parent[v], found[v], wantParent[v], wantFound[v])
			}
		}
	}

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	mallocs := after.Mallocs - before.Mallocs
	if mallocs > primMSTMallocCeiling {
		t.Fatalf("%d cold-pool PrimMST calls allocated %d heap objects, ceiling %d: the heap backing is being grown by append (rmp #2862)",
			dijkstraIntoColdPoolRounds, mallocs, primMSTMallocCeiling)
	}
	t.Logf("%d cold-pool PrimMST calls: %d heap objects (ceiling %d)",
		dijkstraIntoColdPoolRounds, mallocs, primMSTMallocCeiling)
}
