package exec_test

// all_nodes_count_scan_test.go — tests for AllNodesCountScan (#2113 / #2066).
//
// The operator emits exactly one row carrying the live-node count. It reads the
// O(1) direct counter when the walker implements liveNodeCounter, and otherwise
// falls back to a single WalkNodeIDs count pass; both must yield the identical
// value. It spawns no goroutines.

import (
	"context"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// countingWalker is a staticNodeWalker that also exposes the O(1) direct live
// count via LiveNodeCount, exercising the fast path.
type countingWalker struct {
	ids []graph.NodeID
}

func (w *countingWalker) WalkNodeIDs(fn func(graph.NodeID) bool) {
	for _, id := range w.ids {
		if !fn(id) {
			return
		}
	}
}

func (w *countingWalker) LiveNodeCount() (int64, bool) { return int64(len(w.ids)), true }

func drainCountScan(t *testing.T, op exec.Operator) int64 {
	t.Helper()
	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("got %d rows (want 1) of width %d (want 1)", len(rows), lenOr0(rows))
	}
	iv, ok := rows[0][0].(expr.IntegerValue)
	if !ok {
		t.Fatalf("row[0][0] is %T, want IntegerValue", rows[0][0])
	}
	return int64(iv)
}

func lenOr0(rows []exec.Row) int {
	if len(rows) == 0 {
		return 0
	}
	return len(rows[0])
}

// TestAllNodesCountScan_DirectAndFallback proves both the O(1) direct-counter path
// (countingWalker implements liveNodeCounter) and the WalkNodeIDs fallback
// (staticNodeWalker does not) yield the exact live count, with no goroutine leak.
func TestAllNodesCountScan_DirectAndFallback(t *testing.T) {
	defer goleak.VerifyNone(t)

	for _, n := range []int{0, 1, 50, 50_000, 123_456} {
		// Direct O(1) path.
		if got := drainCountScan(t, exec.NewAllNodesCountScan(&countingWalker{ids: makeIDs(n)})); got != int64(n) {
			t.Errorf("direct count = %d, want %d", got, n)
		}
		// Fallback walk-and-count path (staticNodeWalker has no LiveNodeCount).
		if got := drainCountScan(t, exec.NewAllNodesCountScan(buildWalker(n))); got != int64(n) {
			t.Errorf("fallback count = %d, want %d", got, n)
		}
	}
}

// TestAllNodesCountScan_SingleRow proves the operator emits exactly one row and
// then reports end-of-stream, and that Close is a safe no-op.
func TestAllNodesCountScan_SingleRow(t *testing.T) {
	op := exec.NewAllNodesCountScan(&countingWalker{ids: makeIDs(7)})
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var row exec.Row
	ok, err := op.Next(&row)
	if err != nil || !ok {
		t.Fatalf("first Next = (%v, %v), want (true, nil)", ok, err)
	}
	if iv, _ := row[0].(expr.IntegerValue); int64(iv) != 7 {
		t.Fatalf("count = %v, want 7", row[0])
	}
	ok, err = op.Next(&row)
	if ok || err != nil {
		t.Fatalf("second Next = (%v, %v), want (false, nil)", ok, err)
	}
	if err := op.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func makeIDs(n int) []graph.NodeID {
	ids := make([]graph.NodeID, n)
	for i := range ids {
		ids[i] = graph.NodeID(i)
	}
	return ids
}
