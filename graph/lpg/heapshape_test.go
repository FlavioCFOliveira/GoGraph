package lpg

import (
	"fmt"
	"runtime"
	"testing"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// heapshape_test.go — pins the per-resident-node heap SHAPE of the in-memory
// engine (rmp #2650).
//
// # Why this file exists
//
// The 2026-08-27 bottleneck audit recorded B-10: a graph that is merely
// RESIDENT — no mutation, no query in flight — costs the garbage collector
// 29.53 ms per forced collection at 800 000 nodes, carrying ~8.01 heap objects
// per node (docs/audit-bottlenecks-2026-08-27.md §3.5). Nothing in the module
// gated that quantity. A change that added one per-node allocation, or grew one
// per-node struct by a single word, would have raised the standing mark bill on
// every resident graph for the life of every process, and no test would have
// noticed.
//
// # Two instruments, deliberately of different kinds
//
// 1. EXACT STRUCT SIZES. A per-node or per-modification record that grows by one
// machine word costs eight bytes on every node of every resident graph, forever.
// unsafe.Sizeof catches that at the byte, with no measurement noise whatsoever,
// and it is the practice Memgraph applies to its own vertex record:
//
//	static_assert(sizeof(Vertex) == 80, "If this changes documentation needs changing");
//	                                            — memgraph, src/storage/v2/vertex.hpp:69
//
// GoGraph already applied it in three places and never generalised it:
//   - graph/index/nodeset_layout_test.go   NodeSet        == 16
//   - graph/lpg/mvcc_txn_test.go           nodeLabelDelta == 32
//   - graph/lpg/mvcc_props_test.go         nodePropDelta  == 56
//
// This table adopts their form — an exact equality with a message that says a
// change needs a RE-MEASUREMENT rather than a new constant — and extends it to
// the two types that carry the resident per-node cost (propBag, labelBag). The
// two MVCC deltas are restated here so the whole per-node shape is legible in
// one place; the existing assertions are left where they are, because each sits
// beside the cost model it guards.
//
// The fifth type named by rmp #2650, adjlist.adjEntry, is asserted in
// graph/adjlist/heapshape_test.go: it is unexported in a different package, so
// unsafe.Sizeof cannot reach it from here. The two files are one table split by
// Go's visibility rules, not by design.
//
// 2. AN OBJECTS-PER-NODE CEILING. Struct sizes cannot see a NEW allocation —
// a second slice, an extra map entry, a boxed value — which costs a whole heap
// object rather than a few bytes. Measured evidence from rmp #2650 puts one live
// heap object at ~4.2 ns of mark time per collection on the reference host
// (Apple M4, go1.27.0, Green Tea GC on by default), against ~0.52 ns for one
// pointer slot: the object HEADER, not its pointer payload, is what a marginal
// allocation costs. So the object count is the quantity worth gating.
//
// # Why the ceiling is a ceiling, and where its number comes from
//
// docs/test-layers.md is explicit that "a ceiling is the right short-layer shape
// only for a quantity the machine's load cannot inflate". A count of live heap
// objects is exactly such a quantity: it is load-invariant, so unlike a
// wall-clock budget it neither false-reds on a busy host nor needs
// testlayers.RequireQuietMachine — which would make it skip on every `make ci`
// and assert nothing.
//
// The number follows the repo's own worst-observed rule for
// PKG_HARD_BUDGET_OVERRIDES (docs/test-layers.md, "The override rule"): the
// WORST figure ever recorded for the quantity, times a margin, never a number
// fitted to whichever run happened to be measured. Every figure this repo
// records for objects-per-resident-node comes from
// docs/audit-bottlenecks-2026-08-27.md §3.5:
//
//	50 000 nodes   8.06   ← the worst recorded
//	200 000 nodes  8.02
//	800 000 nodes  8.01
//
// so the worst is 8.06, at the smallest of the three graphs — the ratio falls
// as n grows, because the per-graph fixed structures amortise. This test builds
// the smallest of those three sizes deliberately: it is the arm that produced
// the worst figure, so a ceiling that holds here holds at 200k and 800k too.
//
// See heapShapeObjectCeiling for the margin and its justification.
const (
	// heapShapeNodes is the audit's smallest arm (§3.5), reproduced so the
	// measured ratio is directly comparable to the 8.06 recorded there.
	heapShapeNodes = 50_000
	// heapShapeDegree matches the audit's fixture. Edges are part of the
	// per-node object count and dropping them would measure a different
	// quantity from the one the recorded figures describe.
	heapShapeDegree = 4
	// heapShapeObjectCeiling is the WORST recorded objects-per-resident-node
	// figure times a 10% margin, as the worst-observed rule in
	// docs/test-layers.md requires — never a number fitted to one run.
	//
	// Every observation of this quantity, all at 50 000 nodes and degree 4:
	//
	//	8.06   docs/audit-bottlenecks-2026-08-27.md §3.5   (the recorded figure)
	//	8.0590 / 8.0587 / 8.0592   rmp #2650, bench/audit352 binary, default build
	//	8.0590 / 8.0592 / 8.0593   rmp #2650, same binary, GOEXPERIMENT=nogreenteagc
	//	8.052                      rmp #2650, this test alone in graph/lpg
	//	8.056                      rmp #2650, this test in the full graph/lpg suite under -race
	//
	// Eight independent processes, total spread 0.09%. The worst is 8.06, so the
	// ceiling is 8.06 x 1.10 = 8.87.
	//
	// The margin is 10%, not the 25% that rule uses for a per-package WALL-CLOCK
	// budget, and the difference is deliberate: 25% was chosen there against a
	// measured 10.5% run-to-run spread in a timing statistic. This one is a COUNT
	// and its measured spread is 0.09%, so 25% would buy nothing and would blind
	// the gate. 10% still trips on one added object per node (+12.4%), which is
	// the smallest regression worth catching.
	//
	// What this ceiling deliberately does NOT tolerate. The measured 8.05 depends
	// on Go's tiny allocator coalescing two of the per-node noscan allocations —
	// the property stream and the boxed integer — into ONE heap object; the
	// inuse_objects attribution under rmp #2650 accounts for nine allocation sites
	// summing to ~9 allocations but only 8.01 live objects. A toolchain or platform
	// that stopped merging them would read ~9.05 and TRIP this gate. That is
	// correct: it would be a real 12% rise in resident objects per node, and the
	// right response is a human re-measurement, not a wider constant.
	heapShapeObjectCeiling = 8.87
)

// TestHeapShape_PerNode is the table described above: exact sizes for every
// per-node and per-modification record this package owns, then a ceiling on the
// heap objects a resident node actually costs.
//
// It does NOT call t.Parallel. MemStats.HeapObjects is process-global, so the
// measurement below is only meaningful while nothing else in the binary is
// building a heap alongside it.
func TestHeapShape_PerNode(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  uintptr
		want uintptr
		why  string
	}{
		{
			name: "propBag",
			got:  unsafe.Sizeof(propBag{}),
			want: 32,
			why: "a byte-stream slice header (24) plus the promoted-map pointer (8). " +
				"It is the per-node property container, so one extra word here is 8 bytes " +
				"on every node of every resident graph",
		},
		{
			name: "labelBag",
			got:  unsafe.Sizeof(labelBag{}),
			want: 40,
			why: "map pointer (8) + ids slice header (24) + the inline singleton LabelID " +
				"(uint32) + count (uint8), padded to 40. The singleton state exists precisely " +
				"so the commonest case — one label on a node — allocates NOTHING beyond this " +
				"struct, and that is what keeps a labelled node off the 8-objects-per-node " +
				"count twice over",
		},
		{
			name: "nodeLabelDelta",
			got:  unsafe.Sizeof(nodeLabelDelta{}),
			want: 32,
			why: "restates graph/lpg/mvcc_txn_test.go's TestNodeLabelDelta_StaysSmall so " +
				"the per-node shape reads as one table; that test remains the primary gate",
		},
		{
			name: "nodePropDelta",
			got:  unsafe.Sizeof(nodePropDelta{}),
			want: 56,
			why: "restates graph/lpg/mvcc_props_test.go's TestNodePropDelta_SizeIsPinned " +
				"for the same reason",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("unsafe.Sizeof(%s{}) = %d, want %d.\n"+
					"Reason the size is pinned: %s.\n"+
					"A change here is a change to the module's resident memory cost per node. "+
					"It needs a re-measurement of the B-10 figures in "+
					"docs/audit-bottlenecks-2026-08-27.md §3.5, not a new constant here.",
					tc.name, tc.got, tc.want, tc.why)
			}
		})
	}

	t.Run("objects-per-resident-node", func(t *testing.T) {
		ratio, before, after := measureObjectsPerNode(t, heapShapeNodes, heapShapeDegree)
		t.Logf("resident nodes %d, degree %d: heap objects %d -> %d, delta %d, "+
			"%.3f objects per node (ceiling %.2f, worst recorded 8.06)",
			heapShapeNodes, heapShapeDegree, before, after, after-before, ratio,
			heapShapeObjectCeiling)
		if ratio > heapShapeObjectCeiling {
			t.Fatalf("objects per resident node = %.3f, ceiling %.2f.\n"+
				"The mark phase re-traces every one of these on every collection for as "+
				"long as the process holds the graph, at ~4.2 ns per object measured under "+
				"rmp #2650. A rise here is a standing GC bill on every resident graph.\n"+
				"If the rise is intended, re-measure the B-10 figures in "+
				"docs/audit-bottlenecks-2026-08-27.md §3.5 and move the ceiling from the new "+
				"worst observation — not from this run.",
				ratio, heapShapeObjectCeiling)
		}
	})
}

// measureObjectsPerNode returns the live heap objects a resident node costs,
// measured as a DELTA across the build so the runtime's own baseline heap — and
// anything an earlier test in this binary left live — cancels instead of being
// amortised into the ratio. The audit's §3.5 figures are deltas measured the
// same way, which is what makes the two comparable.
func measureObjectsPerNode(t *testing.T, nodes, degree int) (ratio float64, before, after uint64) {
	t.Helper()

	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	t.Cleanup(func() {
		if err := g.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	// Bracket the observation: two collections settle the sweep before each
	// reading, so neither figure includes garbage the other run created.
	before = liveHeapObjects()

	for i := 0; i < nodes; i++ {
		k := fmt.Sprintf("n%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%s): %v", k, err)
		}
		if err := g.SetNodeLabel(k, "Person"); err != nil {
			t.Fatalf("SetNodeLabel(%s): %v", k, err)
		}
		if err := g.SetNodeProperty(k, "age", Int64Value(int64(1000+i%60000))); err != nil {
			t.Fatalf("SetNodeProperty(%s): %v", k, err)
		}
	}
	for i := 0; i < nodes; i++ {
		src := fmt.Sprintf("n%d", i)
		for d := 1; d <= degree; d++ {
			dst := fmt.Sprintf("n%d", (i+d*104_729)%nodes)
			if err := g.AddEdge(src, dst, 0); err != nil {
				t.Fatalf("AddEdge(%s,%s): %v", src, dst, err)
			}
			// SetEdgeLabel returns no error, exactly as the audit's own fixture in
			// bench/audit352/gctax_soak_test.go calls it.
			g.SetEdgeLabel(src, dst, "KNOWS")
		}
	}

	after = liveHeapObjects()
	// KeepAlive is load-bearing: without it the graph is dead by the second
	// reading and the test would measure an empty heap and always pass.
	runtime.KeepAlive(g)
	return float64(after-before) / float64(nodes), before, after
}

// liveHeapObjects reports MemStats.HeapObjects on a quiesced heap. Two
// collections, because the first also sweeps what the caller just built and
// HeapObjects is only a live count once the sweep has caught up.
func liveHeapObjects() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapObjects
}
