package exec_test

// expand_dst_admit_test.go — the far-endpoint admission gate (rmp #2629).
//
// [exec.Expand] documents that its row path ([exec.Expand.Next]) and its columnar
// path (FillChunk over [exec.NewColumnarExpand]) are equivalent. A gate applied on
// only ONE of the two would break that invariant silently: the operator would
// return a different multiset depending on which path its parent happened to
// drive, and no answer-equivalence test written at the query level would catch it
// because the planner picks the path.
//
// So every case here drives the SAME gated traversal both ways and asserts a
// byte-identical multiset, and asserts separately that the gate ACTUALLY REJECTED
// something — a gate that admits everything would pass an equivalence test
// trivially and prove nothing.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// admitSet returns a [exec.DstAdmit] admitting exactly the listed destinations and
// counting how many calls it refused, so a case can prove the gate did work.
func admitSet(refused *int, allow ...int) exec.DstAdmit {
	set := make(map[graph.NodeID]struct{}, len(allow))
	for _, id := range allow {
		set[graph.NodeID(id)] = struct{}{}
	}
	return func(dst graph.NodeID) bool {
		if _, ok := set[dst]; ok {
			return true
		}
		*refused++
		return false
	}
}

// drainGatedRow drains a row-mode Expand carrying gate.
func drainGatedRow(t *testing.T, ids []int64, fwd, rev *staticCSR, filter map[uint64]string, cfg exec.ExpandConfig, gate exec.DstAdmit) []exec.Row {
	t.Helper()
	op := exec.NewExpand(&nodeIDChunkSource{ids: ids}, exec.StaticAdjacency(fwd, rev, filter), cfg).WithDstAdmit(gate)
	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("row-mode Drain: %v", err)
	}
	return rows
}

// drainGatedChunk drains a columnarExpand carrying gate, with the given per-call
// cap and child-batch size so both cursor levels and their cross-call resume are
// crossed while the gate is rejecting.
func drainGatedChunk(t *testing.T, ids []int64, fwd, rev *staticCSR, filter map[uint64]string, cfg exec.ExpandConfig, gate exec.DstAdmit, perCall, batch int) []exec.Row {
	t.Helper()
	base := exec.NewExpand(&nodeIDChunkSource{ids: ids, batch: batch}, exec.StaticAdjacency(fwd, rev, filter), cfg).WithDstAdmit(gate)
	cp, ok := exec.NewColumnarExpand(base)
	if !ok {
		t.Fatalf("NewColumnarExpand: child was not recognised as a ChunkProducer")
	}
	if err := cp.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	dst := cp.NewOutputChunk(exec.DefaultChunkCapacity)
	for {
		n, err := cp.FillChunk(dst, perCall)
		if err != nil {
			t.Fatalf("FillChunk: %v", err)
		}
		if n < perCall {
			break // n < perCall ⇔ end-of-stream
		}
	}
	if err := cp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rows := make([]exec.Row, dst.Len())
	for i := range rows {
		rows[i] = dst.BoxRow(i, nil)
	}
	return rows
}

// assertGatedPathsAgree drives the gated traversal row-mode and then column-major
// across a matrix of per-call caps and child-batch sizes, asserting an identical
// multiset every time. It returns the row-mode multiset so a caller can assert on
// its contents, and fails if the gate never refused a slot on either path.
func assertGatedPathsAgree(t *testing.T, ids []int64, fwd, rev *staticCSR, filter map[uint64]string, cfg exec.ExpandConfig, gate func(refused *int) exec.DstAdmit) []string {
	t.Helper()
	rowRefused := 0
	want := sortedRowKeys(drainGatedRow(t, ids, fwd, rev, filter, cfg, gate(&rowRefused)))
	if rowRefused == 0 {
		t.Fatalf("row mode: the gate refused NOTHING, so this case proves nothing")
	}
	for _, perCall := range []int{1, 2, 3, 7, 64, exec.DefaultChunkCapacity} {
		for _, batch := range []int{0, 1, 2, 5} {
			chunkRefused := 0
			got := sortedRowKeys(drainGatedChunk(t, ids, fwd, rev, filter, cfg, gate(&chunkRefused), perCall, batch))
			if chunkRefused != rowRefused {
				t.Fatalf("perCall=%d batch=%d: gate refused %d slots column-major, %d row-mode",
					perCall, batch, chunkRefused, rowRefused)
			}
			if len(got) != len(want) {
				t.Fatalf("perCall=%d batch=%d: got %d rows, want %d", perCall, batch, len(got), len(want))
			}
			if i, ok := equalKeys(want, got); !ok {
				t.Fatalf("perCall=%d batch=%d: row multiset mismatch at %d:\n want %q\n  got %q",
					perCall, batch, i, want[i], got[i])
			}
		}
	}
	return want
}

func TestExpandDstAdmit_RowAndChunkAgree_DirOut(t *testing.T) {
	fwd := buildCSR(5, [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}, {3, 4}})
	rev := buildCSR(5, [][2]int{{1, 0}, {2, 0}, {3, 1}, {3, 2}, {4, 3}})
	ids := []int64{0, 1, 2, 3, 4}
	// Admit only destinations 1 and 3: 0→1, 1→3, 2→3 survive; 0→2 and 3→4 do not.
	got := assertGatedPathsAgree(t, ids, fwd, rev, nil,
		exec.ExpandConfig{Direction: exec.DirOut, InputCol: 0},
		func(refused *int) exec.DstAdmit { return admitSet(refused, 1, 3) })
	if len(got) != 3 {
		t.Fatalf("gated DirOut emitted %d rows, want 3: %q", len(got), got)
	}
}

func TestExpandDstAdmit_RowAndChunkAgree_DirIn(t *testing.T) {
	// A fan-in star: 0→1, 2→1, 3→1, plus 1→4. Driven IN from every node, so the
	// gate sees the SOURCES of the in-edges as its destinations.
	fwd := buildCSR(5, [][2]int{{0, 1}, {2, 1}, {3, 1}, {1, 4}})
	rev := buildCSR(5, [][2]int{{1, 0}, {1, 2}, {1, 3}, {4, 1}})
	ids := []int64{0, 1, 2, 3, 4}
	assertGatedPathsAgree(t, ids, fwd, rev, nil,
		exec.ExpandConfig{Direction: exec.DirIn, InputCol: 0},
		func(refused *int) exec.DstAdmit { return admitSet(refused, 0, 2) })
}

func TestExpandDstAdmit_RowAndChunkAgree_DirBoth(t *testing.T) {
	// Includes a self-loop (2→2) so the undirected self-loop dedup is crossed
	// while the gate is rejecting.
	fwd := buildCSR(4, [][2]int{{0, 1}, {0, 2}, {1, 2}, {2, 2}, {2, 3}})
	rev := buildCSR(4, [][2]int{{1, 0}, {2, 0}, {2, 1}, {2, 2}, {3, 2}})
	ids := []int64{0, 1, 2, 3}
	assertGatedPathsAgree(t, ids, fwd, rev, nil,
		exec.ExpandConfig{Direction: exec.DirBoth, InputCol: 0},
		func(refused *int) exec.DstAdmit { return admitSet(refused, 2) })
}

func TestExpandDstAdmit_ComposesWithEdgeTypeFilter(t *testing.T) {
	// fwd positions: 0:(0→1) 1:(0→2) 2:(0→3) 3:(1→3). The type filter accepts
	// positions {0,2} — destinations 1 and 3 — and the gate then admits only 3, so
	// exactly one row survives BOTH gates. This is the composition the planner
	// relies on: the type is decided per slot inside the walk, the endpoint on the
	// emit branch, and neither may swallow the other.
	fwd := buildCSR(4, [][2]int{{0, 1}, {0, 2}, {0, 3}, {1, 3}})
	rev := buildCSR(4, [][2]int{{1, 0}, {2, 0}, {3, 0}, {3, 1}})
	filter := map[uint64]string{0: "KNOWS", 2: "KNOWS"}
	ids := []int64{0, 1}
	got := assertGatedPathsAgree(t, ids, fwd, rev, filter,
		exec.ExpandConfig{Direction: exec.DirOut, EdgeType: "KNOWS", InputCol: 0},
		func(refused *int) exec.DstAdmit { return admitSet(refused, 3) })
	if len(got) != 1 {
		t.Fatalf("type filter ∧ endpoint gate emitted %d rows, want 1: %q", len(got), got)
	}
}

// TestExpandDstAdmit_RejectAllYieldsNoRows pins the boundary case the planner's
// "mismatched label" shape produces: a gate that refuses every destination must
// end the stream cleanly on both paths rather than spin, and must leave no row
// behind.
func TestExpandDstAdmit_RejectAllYieldsNoRows(t *testing.T) {
	fwd := buildCSR(5, [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}, {3, 4}})
	rev := buildCSR(5, [][2]int{{1, 0}, {2, 0}, {3, 1}, {3, 2}, {4, 3}})
	ids := []int64{0, 1, 2, 3, 4}
	cfg := exec.ExpandConfig{Direction: exec.DirOut, InputCol: 0}
	refuseAll := func(refused *int) exec.DstAdmit {
		return func(graph.NodeID) bool { *refused++; return false }
	}

	rowRefused := 0
	if rows := drainGatedRow(t, ids, fwd, rev, nil, cfg, refuseAll(&rowRefused)); len(rows) != 0 {
		t.Fatalf("row mode: reject-all emitted %d rows, want 0", len(rows))
	}
	if rowRefused != 5 {
		t.Fatalf("row mode: gate was consulted %d times, want 5 (one per adjacency slot)", rowRefused)
	}
	for _, perCall := range []int{1, 7, exec.DefaultChunkCapacity} {
		chunkRefused := 0
		if rows := drainGatedChunk(t, ids, fwd, rev, nil, cfg, refuseAll(&chunkRefused), perCall, 0); len(rows) != 0 {
			t.Fatalf("perCall=%d: reject-all emitted %d rows, want 0", perCall, len(rows))
		}
		if chunkRefused != 5 {
			t.Fatalf("perCall=%d: gate was consulted %d times, want 5", perCall, chunkRefused)
		}
	}
}

// TestExpandDstAdmit_NilGateAdmitsEverything pins the default: an Expand with no
// gate must emit exactly what it emitted before the gate existed. It is the
// control for every OTHER traversal in the engine, none of which carries a gate.
func TestExpandDstAdmit_NilGateAdmitsEverything(t *testing.T) {
	fwd := buildCSR(5, [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}, {3, 4}})
	rev := buildCSR(5, [][2]int{{1, 0}, {2, 0}, {3, 1}, {3, 2}, {4, 3}})
	ids := []int64{0, 1, 2, 3, 4}
	cfg := exec.ExpandConfig{Direction: exec.DirOut, InputCol: 0}

	ungated := sortedRowKeys(drainExpandRow(t, ids, fwd, rev, nil, cfg))
	explicitNil := sortedRowKeys(drainGatedRow(t, ids, fwd, rev, nil, cfg, nil))
	if i, ok := equalKeys(ungated, explicitNil); !ok {
		t.Fatalf("a nil gate changed the row multiset at %d: want %q, got %q", i, ungated[i], explicitNil[i])
	}
	if len(ungated) != 5 {
		t.Fatalf("ungated DirOut emitted %d rows, want 5", len(ungated))
	}
}
