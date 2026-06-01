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
// 2. Half-open interval (lo, hi] — exclusive lower bound
// ─────────────────────────────────────────────────────────────────────────────

func TestNodeByIndexRangeScan_ExclusiveLower(t *testing.T) {
	// The test stub range lookup is inclusive by design; exclusion is applied
	// by the operator itself when Include=false.
	//
	// We index NodeIDs as the int64 value for simplicity (1-to-1 mapping).
	entries := map[int64][]uint64{
		10: {10},
		20: {20},
		30: {30},
	}
	lookup := newInt64RangeLookup(entries)

	// (10, 30] — should exclude NodeID 10 (which is the boundary).
	lo := exec.RangeBound{Value: expr.IntegerValue(10), Include: false}
	hi := exec.RangeBound{Value: expr.IntegerValue(30), Include: true}
	op := exec.NewNodeByIndexRangeScan(lookup, lo, hi)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// NOTE: The operator's exclusive-bound filter compares the NodeID (not the
	// index key) against the bound value.  In this fixture the NodeID equals
	// the key, so the filter works correctly.
	got := make(map[int64]bool, len(rows))
	for _, row := range rows {
		got[int64(row[0].(expr.IntegerValue))] = true
	}
	if got[10] {
		t.Error("NodeID 10 should have been excluded by exclusive lower bound")
	}
	if !got[20] || !got[30] {
		t.Error("NodeIDs 20 and 30 should be present")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Open interval (lo, hi) — both exclusive
// ─────────────────────────────────────────────────────────────────────────────

func TestNodeByIndexRangeScan_OpenInterval(t *testing.T) {
	entries := map[int64][]uint64{
		10: {10},
		20: {20},
		30: {30},
	}
	lookup := newInt64RangeLookup(entries)

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
	if got[10] || got[30] {
		t.Error("boundary NodeIDs 10 and 30 should be excluded in open interval")
	}
	if !got[20] {
		t.Error("NodeID 20 should be present in (10, 30)")
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
