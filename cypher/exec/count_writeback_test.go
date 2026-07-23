package exec

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/index/count"
)

func TestCountBuffer_CommitAppliesDeltasThenDirty(t *testing.T) {
	cs := count.New(0)
	var b CountBuffer
	b.EnqueueDelta(count.EDelta(10, 1))
	b.EnqueueDelta(count.DDelta(1, 10, count.Out, 1))
	b.EnqueueDelta(count.TDelta(1, 10, 2, 1))
	b.MarkDirty(count.DirtyMark{Label: 2, Scope: count.DirtyTB})
	if b.Len() != 4 {
		t.Fatalf("Len = %d, want 4", b.Len())
	}
	b.Commit(cs)
	if b.Len() != 0 {
		t.Fatalf("Len after Commit = %d, want 0 (reset)", b.Len())
	}
	if cs.CountE(10) != 1 || cs.CountD(1, 10, count.Out) != 1 || cs.CountT(1, 10, 2) != 1 {
		t.Fatalf("Commit did not apply deltas")
	}
	if !cs.TDirty(1, 2) {
		t.Fatalf("Commit did not apply dirty marking")
	}
}

func TestCountBuffer_RollbackDiscards(t *testing.T) {
	cs := count.New(0)
	var b CountBuffer
	b.EnqueueDelta(count.EDelta(10, 1))
	b.MarkDirty(count.DirtyMark{Label: 2, Scope: count.DirtyTB})
	b.Rollback()
	if b.Len() != 0 {
		t.Fatalf("Len after Rollback = %d, want 0", b.Len())
	}
	if cs.CountE(10) != 0 || cs.TDirty(1, 2) {
		t.Fatalf("Rollback must not touch the store")
	}
}

func TestCountBuffer_CommitNilStoreSafe(t *testing.T) {
	var b CountBuffer
	b.EnqueueDelta(count.EDelta(10, 1))
	b.Commit(nil) // must not panic
	if b.Len() != 0 {
		t.Fatalf("Len after nil Commit = %d, want 0", b.Len())
	}
}
