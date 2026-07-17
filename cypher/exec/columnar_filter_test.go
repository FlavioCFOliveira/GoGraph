package exec_test

// columnar_filter_test.go — operator-level tests for ColumnarFilter (#1704 P3).
//
// These exercise the mechanics the byte-identity engine test cannot easily reach:
// the persistent scratch cursor across FillChunk calls, row compaction across scan
// batch boundaries, the n<maxRows⇔EOS drain contract, the boxed-predicate fallback,
// and the reversible boxed Next path.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// keepEvenPred is the unboxed predicate: keep rows whose int64 NodeID is even.
func keepEvenPred(src *exec.Chunk, row int) (keep, decided bool) {
	v, valid := src.Int64(0, row)
	if !valid {
		return false, false
	}
	return v%2 == 0, true
}

// keepEvenBoxed is the byte-identical boxed fallback predicate.
func keepEvenBoxed(row exec.Row) (expr.Value, error) {
	iv, ok := row[0].(expr.IntegerValue)
	if !ok {
		return expr.Null, nil
	}
	return expr.BoolValue(int64(iv)%2 == 0), nil
}

// drainColumnarFilter drives cf.FillChunk with the given per-call maxRows until
// end-of-stream (a short return) and returns every surviving int64 in order.
func drainColumnarFilter(t *testing.T, cf *exec.ColumnarFilter, maxRows int) []int64 {
	t.Helper()
	if err := cf.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	dst := cf.NewOutputChunk(exec.DefaultChunkCapacity)
	for {
		before := dst.Len()
		n, err := cf.FillChunk(dst, maxRows)
		if err != nil {
			t.Fatalf("FillChunk: %v", err)
		}
		if got := dst.Len() - before; got != n {
			t.Fatalf("FillChunk returned n=%d but appended %d rows", n, got)
		}
		if n < maxRows {
			break // n < maxRows ⇔ end-of-stream
		}
	}
	out := make([]int64, dst.Len())
	for i := range out {
		v, valid := dst.Int64(0, i)
		if !valid {
			t.Fatalf("row %d unexpectedly NULL", i)
		}
		out[i] = v
	}
	return out
}

// wantEvens returns the even NodeIDs in [0,n) in order — the expected survivors.
func wantEvens(n int) []int64 {
	var out []int64
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			out = append(out, int64(i))
		}
	}
	return out
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestColumnarFilter_Compaction_Cursor drives a highly selective filter over a
// scan far larger than one chunk with a small per-call maxRows, so the scratch
// cursor must persist across FillChunk calls and across scan batch boundaries
// without dropping or duplicating a survivor.
func TestColumnarFilter_Compaction_Cursor(t *testing.T) {
	const n = 10_000
	for _, maxRows := range []int{1, 7, 100, 4096, 100_000} {
		walker := buildWalker(n)
		cf := exec.NewColumnarFilter(exec.NewAllNodesScan(walker), keepEvenBoxed, keepEvenPred)
		got := drainColumnarFilter(t, cf, maxRows)
		if want := wantEvens(n); !equalInt64s(got, want) {
			t.Fatalf("maxRows=%d: got %d survivors, want %d (first mismatch check)", maxRows, len(got), len(want))
		}
	}
}

// TestColumnarFilter_FallbackPath drives the filter with a nil unboxed predicate,
// forcing every row through the boxed fallback (box-one-row + boxed predFn). The
// compaction must still be exact.
func TestColumnarFilter_FallbackPath(t *testing.T) {
	const n = 5_000
	walker := buildWalker(n)
	cf := exec.NewColumnarFilter(exec.NewAllNodesScan(walker), keepEvenBoxed, nil)
	got := drainColumnarFilter(t, cf, 128)
	if want := wantEvens(n); !equalInt64s(got, want) {
		t.Fatalf("fallback path: got %d survivors, want %d", len(got), len(want))
	}
}

// TestColumnarFilter_BoxedNext_Reversible drives the filter through the boxed Next
// path (a non-columnar parent) and confirms it yields the same survivors — the
// reversibility contract.
func TestColumnarFilter_BoxedNext_Reversible(t *testing.T) {
	const n = 3_000
	walker := buildWalker(n)
	cf := exec.NewColumnarFilter(exec.NewAllNodesScan(walker), keepEvenBoxed, keepEvenPred)
	rows, err := exec.Drain(context.Background(), cf)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	got := make([]int64, len(rows))
	for i, row := range rows {
		iv, ok := row[0].(expr.IntegerValue)
		if !ok {
			t.Fatalf("row %d col 0 is %T, want IntegerValue", i, row[0])
		}
		got[i] = int64(iv)
	}
	if want := wantEvens(n); !equalInt64s(got, want) {
		t.Fatalf("boxed Next: got %d survivors, want %d", len(got), len(want))
	}
}

// TestAllNodesScan_FillChunk confirms the scan's ChunkProducer emits its NodeIDs
// as an unboxed int64 column, in order, matching the boxed Next path.
func TestAllNodesScan_FillChunk(t *testing.T) {
	ids := []graph.NodeID{0, 1, 255, 256, 1000, 1 << 40}
	walker := &staticNodeWalker{ids: ids}
	scan := exec.NewAllNodesScan(walker)
	if err := scan.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	dst := scan.NewOutputChunk(0)
	total := 0
	for {
		n, err := scan.FillChunk(dst, 2) // small batch to force multiple pulls
		if err != nil {
			t.Fatalf("FillChunk: %v", err)
		}
		total += n
		if n < 2 {
			break
		}
	}
	if total != len(ids) {
		t.Fatalf("got %d rows, want %d", total, len(ids))
	}
	if !dst.IsInt64Column(0) {
		t.Fatalf("scan output column 0 is not an unboxed int64 column")
	}
	for i, id := range ids {
		v, valid := dst.Int64(0, i)
		if !valid || v != int64(id) {
			t.Fatalf("row %d: got (%d,%v), want %d", i, v, valid, int64(id))
		}
	}
}
