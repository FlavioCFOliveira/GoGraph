package exec_test

// scan_all_test.go — tests for AllNodesScan (task-236).

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// staticNodeWalker is a test stub that returns a fixed set of NodeIDs.
type staticNodeWalker struct {
	ids []graph.NodeID
}

func (w *staticNodeWalker) WalkNodeIDs(fn func(graph.NodeID) bool) {
	for _, id := range w.ids {
		if !fn(id) {
			return
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. AllNodesScan — basic iteration
// ─────────────────────────────────────────────────────────────────────────────

func TestAllNodesScan_Basic(t *testing.T) {
	ids := []graph.NodeID{0, 1, 2, 3, 4}
	walker := &staticNodeWalker{ids: ids}
	op := exec.NewAllNodesScan(walker)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != len(ids) {
		t.Fatalf("got %d rows, want %d", len(rows), len(ids))
	}
	// Collect returned IDs.
	got := make(map[int64]struct{}, len(rows))
	for _, row := range rows {
		iv, ok := row[0].(expr.IntegerValue)
		if !ok {
			t.Fatalf("row[0] is %T, want expr.IntegerValue", row[0])
		}
		got[int64(iv)] = struct{}{}
	}
	for _, id := range ids {
		if _, ok := got[int64(id)]; !ok {
			t.Errorf("NodeID %d missing from output", id)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. AllNodesScan — empty graph
// ─────────────────────────────────────────────────────────────────────────────

func TestAllNodesScan_Empty(t *testing.T) {
	op := exec.NewAllNodesScan(&staticNodeWalker{})
	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(rows))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. AllNodesScan — cancellation
// ─────────────────────────────────────────────────────────────────────────────

func TestAllNodesScan_Cancellation(t *testing.T) {
	// Build a large-ish walker that would never complete without cancellation.
	ids := make([]graph.NodeID, 1_000_000)
	for i := range ids {
		ids[i] = graph.NodeID(i)
	}
	walker := &staticNodeWalker{ids: ids}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before even starting

	op := exec.NewAllNodesScan(walker)
	_, err := exec.Drain(ctx, op)
	if err == nil {
		t.Fatal("expected error after context cancellation, got nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. AllNodesScan — Close always called
// ─────────────────────────────────────────────────────────────────────────────

func TestAllNodesScan_CloseAlwaysCalled(t *testing.T) {
	walker := &staticNodeWalker{ids: []graph.NodeID{10, 20}}
	op := exec.NewAllNodesScan(walker)

	ctx := context.Background()
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var row exec.Row
	if ok, err := op.Next(&row); !ok || err != nil {
		t.Fatalf("first Next = (%v, %v)", ok, err)
	}
	// Close before draining all rows.
	if err := op.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. AllNodesScan — per-scanned-node allocation cost (benchmark)
// ─────────────────────────────────────────────────────────────────────────────

// BenchmarkAllNodesScan_PerNodeAllocCost measures AllNodesScan's real
// allocation cost on a fixture large enough (1000 NodeIDs) to exceed Go's
// staticuint64s fast path (small integers 0-255 box without a heap
// allocation), unlike a fixture confined to IDs < 256, which would report
// 0 allocs/op regardless of whether the operator is actually zero-alloc.
// This benchmark measures ~759 allocs/op — AllNodesScan is NOT zero-alloc
// for realistic NodeID ranges; the scan leaf boxes one expr.IntegerValue per
// scanned node (2026-07-02 production-readiness audit finding P1). This
// benchmark was previously named BenchmarkAllNodesScan_ZeroAlloc, which
// claimed a property its own measured numbers contradicted (finding P4).
func BenchmarkAllNodesScan_PerNodeAllocCost(b *testing.B) {
	const n = 1000
	ids := make([]graph.NodeID, n)
	for i := range ids {
		ids[i] = graph.NodeID(i)
	}
	walker := &staticNodeWalker{ids: ids}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		op := exec.NewAllNodesScan(walker)
		if err := op.Init(ctx); err != nil {
			b.Fatal(err)
		}
		var row exec.Row
		for {
			ok, err := op.Next(&row)
			if err != nil {
				b.Fatal(err)
			}
			if !ok {
				break
			}
		}
		_ = op.Close()
	}
}
