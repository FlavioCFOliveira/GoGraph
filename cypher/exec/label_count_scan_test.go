package exec_test

// label_count_scan_test.go — unit tests for LabelCountScan (#2004).
//
// These exercise the operator in isolation: the zero-alloc direct-count fast
// path (a resolver implementing the optional ResolveLabelCount), the
// bitmap-cardinality fallback (a resolver exposing only ResolveLabelBitmap), the
// single-row/end-of-stream lifecycle, and context cancellation.

import (
	"context"
	"testing"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// countingLabelResolver returns a fixed direct count AND a bitmap whose
// cardinality DIFFERS, so a test can tell which path the operator took: the
// direct-count value is returned iff the operator used ResolveLabelCount.
type countingLabelResolver struct {
	count      int64
	countOK    bool
	bitmapCard int // cardinality of the bitmap ResolveLabelBitmap returns
}

func (r *countingLabelResolver) ResolveLabelBitmap(string) *roaring64.Bitmap {
	bm := roaring64.New()
	for i := 0; i < r.bitmapCard; i++ {
		bm.Add(uint64(i))
	}
	return bm
}

func (r *countingLabelResolver) ResolveLabelCount(string) (int64, bool) {
	return r.count, r.countOK
}

func drainLabelCount(t *testing.T, op *exec.LabelCountScan) int64 {
	t.Helper()
	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want exactly 1", len(rows))
	}
	iv, ok := rows[0][0].(expr.IntegerValue)
	if !ok {
		t.Fatalf("row[0] is %T, want IntegerValue", rows[0][0])
	}
	return int64(iv)
}

// TestLabelCountScan_DirectCount proves the operator uses the zero-alloc direct
// count when the resolver supports it (returns the direct value, not the bitmap
// cardinality).
func TestLabelCountScan_DirectCount(t *testing.T) {
	// count=42 via the direct path; bitmap cardinality is 3 — a mismatch that
	// proves which path ran.
	r := &countingLabelResolver{count: 42, countOK: true, bitmapCard: 3}
	got := drainLabelCount(t, exec.NewLabelCountScan("Item", r))
	if got != 42 {
		t.Fatalf("count = %d, want 42 (direct-count path)", got)
	}
}

// TestLabelCountScan_BitmapFallback proves the operator falls back to the bitmap
// cardinality when the resolver's direct count reports it cannot answer.
func TestLabelCountScan_BitmapFallback(t *testing.T) {
	// Direct count declines (countOK=false); bitmap cardinality (7) must win.
	r := &countingLabelResolver{count: 42, countOK: false, bitmapCard: 7}
	got := drainLabelCount(t, exec.NewLabelCountScan("Item", r))
	if got != 7 {
		t.Fatalf("count = %d, want 7 (bitmap-cardinality fallback)", got)
	}
}

// TestLabelCountScan_BitmapOnlyResolver proves a resolver that implements ONLY
// ResolveLabelBitmap (no ResolveLabelCount method at all) takes the fallback.
func TestLabelCountScan_BitmapOnlyResolver(t *testing.T) {
	r := &staticLabelResolver{label: "Person", nodeIDs: []uint64{2, 5, 9, 11}}
	got := drainLabelCount(t, exec.NewLabelCountScan("Person", r))
	if got != 4 {
		t.Fatalf("count = %d, want 4", got)
	}
}

// TestLabelCountScan_ZeroCount proves an empty/unknown label yields a single
// row carrying 0.
func TestLabelCountScan_ZeroCount(t *testing.T) {
	r := &countingLabelResolver{count: 0, countOK: true, bitmapCard: 0}
	got := drainLabelCount(t, exec.NewLabelCountScan("Ghost", r))
	if got != 0 {
		t.Fatalf("count = %d, want 0", got)
	}
}

// TestLabelCountScan_EndOfStream proves the operator emits exactly one row and
// then reports end-of-stream on subsequent Next calls.
func TestLabelCountScan_EndOfStream(t *testing.T) {
	r := &countingLabelResolver{count: 5, countOK: true, bitmapCard: 0}
	op := exec.NewLabelCountScan("Item", r)
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var row exec.Row
	ok, err := op.Next(&row)
	if err != nil || !ok {
		t.Fatalf("first Next = (%v, %v), want (true, nil)", ok, err)
	}
	ok, err = op.Next(&row)
	if err != nil || ok {
		t.Fatalf("second Next = (%v, %v), want (false, nil)", ok, err)
	}
	if err := op.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestLabelCountScan_Cancellation proves a cancelled context is honoured at Init.
func TestLabelCountScan_Cancellation(t *testing.T) {
	r := &countingLabelResolver{count: 5, countOK: true, bitmapCard: 0}
	op := exec.NewLabelCountScan("Item", r)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := op.Init(ctx); err == nil {
		t.Fatal("Init on a cancelled context = nil, want context.Canceled")
	}
	_ = op.Close()
}
