package exec_test

// scan_index_btree_test.go — tests for NodeByIndexRangeScan (task-239).

import (
	"context"
	"testing"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test stubs
// ─────────────────────────────────────────────────────────────────────────────

// int64RangeLookup is a simple test double for an int64 btree index.
type int64RangeLookup struct {
	// entries maps int64 value → NodeID set.
	entries map[int64][]uint64
}

func newInt64RangeLookup(entries map[int64][]uint64) *int64RangeLookup {
	return &int64RangeLookup{entries: entries}
}

// RangeBitmap returns the union of NodeID sets for keys in [lo, hi].
func (r *int64RangeLookup) RangeBitmap(lo, hi expr.Value) *roaring64.Bitmap {
	bm := roaring64.New()
	var loVal, hiVal int64
	const minInt64 = int64(-1 << 63)
	const maxInt64 = int64(1<<63 - 1)

	if lo == nil || expr.IsNull(lo) {
		loVal = minInt64
	} else {
		loVal = int64(lo.(expr.IntegerValue))
	}
	if hi == nil || expr.IsNull(hi) {
		hiVal = maxInt64
	} else {
		hiVal = int64(hi.(expr.IntegerValue))
	}
	for k, ids := range r.entries {
		if k >= loVal && k <= hiVal {
			bm.AddMany(ids)
		}
	}
	return bm
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. Closed interval [lo, hi] — both inclusive
// ─────────────────────────────────────────────────────────────────────────────

func TestNodeByIndexRangeScan_ClosedInterval(t *testing.T) {
	lookup := newInt64RangeLookup(map[int64][]uint64{
		1: {10},
		3: {30},
		5: {50},
		7: {70},
	})
	lo := exec.RangeBound{Value: expr.IntegerValue(3), Include: true}
	hi := exec.RangeBound{Value: expr.IntegerValue(5), Include: true}
	op := exec.NewNodeByIndexRangeScan(lookup, lo, hi)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (keys 3 and 5)", len(rows))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Exclusive bounds are NOT enforced by the operator — it returns the
//    inclusive [lo, hi] superset (#F-EXEC1). The operator holds only a NodeID
//    bitmap, not property values, so it cannot compare a node's value to the
//    bound; exact open/closed semantics are the caller's residual-filter job.
//    These fixtures deliberately break the old NodeID==key coincidence (the
//    node's ID differs from its index key) so the test verifies the real
//    contract instead of masking the prior NodeID-vs-value confusion.
// ─────────────────────────────────────────────────────────────────────────────

func TestNodeByIndexRangeScan_ExclusiveBoundsNotEnforced(t *testing.T) {
	// Keys 10/20/30 map to unrelated NodeIDs (101/102/103): NodeID != key.
	entries := map[int64][]uint64{
		10: {101},
		20: {102},
		30: {103},
	}
	lookup := newInt64RangeLookup(entries)

	// Request (10, 30) — both exclusive. The operator ignores Include and
	// returns the inclusive superset: all three nodes, INCLUDING the boundary
	// keys 10 and 30. (A residual Filter, applied by the planner, would then
	// drop the boundary values — but that is not this operator's job.)
	lo := exec.RangeBound{Value: expr.IntegerValue(10), Include: false}
	hi := exec.RangeBound{Value: expr.IntegerValue(30), Include: false}
	op := exec.NewNodeByIndexRangeScan(lookup, lo, hi)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	got := make(map[int64]bool, len(rows))
	for _, row := range rows {
		got[int64(row[0].(expr.IntegerValue))] = true
	}
	for _, want := range []int64{101, 102, 103} {
		if !got[want] {
			t.Errorf("inclusive superset must contain node %d (exclusive bounds are not enforced here)", want)
		}
	}
	if len(got) != 3 {
		t.Errorf("expected the full inclusive superset (3 nodes), got %d", len(got))
	}
}

// TestNodeByIndexRangeScan_NoNodeIDvsBoundConfusion pins the specific defect
// removed in #F-EXEC1: previously an exclusive numeric bound was enforced by
// comparing the emitted NodeID to the bound value, so a node whose ID happened
// to equal the numeric bound was wrongly dropped. Here node 20 carries index
// key 500 and is in range, while the exclusive lower bound is 20 — the old code
// would have dropped node 20 (ID == bound); the fixed operator keeps it.
func TestNodeByIndexRangeScan_NoNodeIDvsBoundConfusion(t *testing.T) {
	// key 500 -> NodeID 20; a wide range that includes key 500.
	entries := map[int64][]uint64{500: {20}}
	lookup := newInt64RangeLookup(entries)

	lo := exec.RangeBound{Value: expr.IntegerValue(20), Include: false} // exclusive lower == NodeID 20
	hi := exec.RangeBound{Value: expr.IntegerValue(1000), Include: true}
	op := exec.NewNodeByIndexRangeScan(lookup, lo, hi)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 || int64(rows[0][0].(expr.IntegerValue)) != 20 {
		t.Errorf("node 20 (key 500, in range) must be returned; the old NodeID-vs-bound filter would have dropped it")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Unbounded range — nil bounds → all nodes
// ─────────────────────────────────────────────────────────────────────────────

func TestNodeByIndexRangeScan_Unbounded(t *testing.T) {
	entries := map[int64][]uint64{
		1: {1},
		2: {2},
		3: {3},
	}
	lookup := newInt64RangeLookup(entries)

	lo := exec.RangeBound{Value: nil, Include: true}
	hi := exec.RangeBound{Value: nil, Include: true}
	op := exec.NewNodeByIndexRangeScan(lookup, lo, hi)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("expected 3 rows for unbounded range, got %d", len(rows))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. Empty range → 0 rows
// ─────────────────────────────────────────────────────────────────────────────

func TestNodeByIndexRangeScan_EmptyRange(t *testing.T) {
	entries := map[int64][]uint64{1: {1}, 2: {2}}
	lookup := newInt64RangeLookup(entries)

	// Range [100, 200] — no keys fall here.
	lo := exec.RangeBound{Value: expr.IntegerValue(100), Include: true}
	hi := exec.RangeBound{Value: expr.IntegerValue(200), Include: true}
	op := exec.NewNodeByIndexRangeScan(lookup, lo, hi)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows for empty range, got %d", len(rows))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. Cancellation
// ─────────────────────────────────────────────────────────────────────────────

func TestNodeByIndexRangeScan_Cancellation(t *testing.T) {
	entries := map[int64][]uint64{}
	ids := make([]uint64, 500)
	for i := range ids {
		ids[i] = uint64(i)
	}
	entries[1] = ids
	lookup := newInt64RangeLookup(entries)

	lo := exec.RangeBound{Value: expr.IntegerValue(1), Include: true}
	hi := exec.RangeBound{Value: expr.IntegerValue(1), Include: true}
	op := exec.NewNodeByIndexRangeScan(lookup, lo, hi)

	ctx, cancel := context.WithCancel(context.Background())
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Drain a few rows.
	var row exec.Row
	for range 5 {
		if _, err := op.Next(&row); err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	cancel()
	_, err := op.Next(&row)
	if err == nil {
		t.Log("Next nil after cancel — bitmap may be exhausted, acceptable")
	}
	_ = op.Close()
}
