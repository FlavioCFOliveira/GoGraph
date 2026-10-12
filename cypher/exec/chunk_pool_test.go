package exec

// chunk_pool_test.go — regression tests for the node-scan chunk pool (rmp #3114).
//
// The pool recycles the single-integer-column chunk a node scan hands out. These
// tests pin the three properties reuse must keep: a recycled chunk carries no
// cell, NULL or length from its previous user; only the pooled shape is admitted;
// and a chunk a consumer still holds is never handed to anyone else.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// poolWalker returns a node walker over the ids [0, n).
func poolWalker(n int) *removedWalker {
	ids := make([]graph.NodeID, n)
	for i := range ids {
		ids[i] = graph.NodeID(i)
	}
	return &removedWalker{ids: ids}
}

func poolKeepEven(src *Chunk, row int) (keep, decided bool) {
	v, valid := src.Int64(0, row)
	if !valid {
		return false, false
	}
	return v%2 == 0, true
}

func poolKeepEvenBoxed(row Row) (expr.Value, error) {
	iv, ok := row[0].(expr.IntegerValue)
	if !ok {
		return expr.Null, nil
	}
	return expr.BoolValue(int64(iv)%2 == 0), nil
}

// TestNodeIDChunkPool_ReuseCarriesNoState fills, verifies and releases a pooled
// chunk many times, with different values and NULL positions each round. Every
// chunk the pool returns must be empty and all-valid, and must hold exactly the
// round's values once filled.
func TestNodeIDChunkPool_ReuseCarriesNoState(t *testing.T) {
	for round := 0; round < 32; round++ {
		c := newNodeIDChunk(DefaultChunkCapacity)
		if c.Len() != 0 || c.Cap() != DefaultChunkCapacity || !c.cols[0].allValid {
			t.Fatalf("round %d: pooled chunk not reset: len=%d cap=%d allValid=%v",
				round, c.Len(), c.Cap(), c.cols[0].allValid)
		}
		rows := DefaultChunkCapacity - round // never grows the backing
		for r := 0; r < rows; r++ {
			if round%2 == 1 && r%7 == 0 {
				c.AppendNull(0)
				continue
			}
			c.AppendInt64(0, int64(round*100_000+r))
		}
		for r := 0; r < rows; r++ {
			v, valid := c.Int64(0, r)
			wantNull := round%2 == 1 && r%7 == 0
			if valid == wantNull || (valid && v != int64(round*100_000+r)) {
				t.Fatalf("round %d row %d: got (%d, %v)", round, r, v, valid)
			}
		}
		releaseChunk(c)
	}
}

// TestReleaseChunk_AdmitsOnlyPooledShape releases chunks of every other shape and
// confirms the pool never returns one of them.
func TestReleaseChunk_AdmitsOnlyPooledShape(t *testing.T) {
	grown := NewChunk(DefaultChunkCapacity, expr.KindInteger)
	for r := 0; r <= DefaultChunkCapacity; r++ {
		grown.AppendInt64(0, int64(r)) // one past capacity: the backing grows
	}
	rejected := []*Chunk{
		grown,
		NewChunk(64, expr.KindInteger),
		NewChunk(DefaultChunkCapacity, expr.KindFloat),
		NewChunk(DefaultChunkCapacity, expr.KindInteger, expr.KindInteger),
		NewDynamicChunk(DefaultChunkCapacity, 1),
	}
	for _, c := range rejected {
		releaseChunk(c)
	}
	releaseChunk(nil)
	for i := 0; i < 64; i++ {
		got, ok := nodeIDChunkPool.Get().(*Chunk)
		if !ok {
			break
		}
		for j, c := range rejected {
			if got == c {
				t.Fatalf("pool returned rejected chunk %d", j)
			}
		}
	}
}

// countEvens runs one count(*) statement — CountRows over a ColumnarFilter over a
// node scan of n ids — and returns the count, closing the plan as the engine does.
func countEvens(t *testing.T, n int) int64 {
	t.Helper()
	op := NewCountRows(NewColumnarFilter(NewAllNodesScan(poolWalker(n)), poolKeepEvenBoxed, poolKeepEven))
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var row Row
	ok, err := op.Next(&row)
	if err != nil || !ok {
		t.Fatalf("Next: ok=%v err=%v", ok, err)
	}
	got := int64(row[0].(expr.IntegerValue))
	if err := op.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return got
}

// TestCountRows_PooledChunksAcrossStatements interleaves statements of different
// sizes — under one batch, exactly one batch, several batches — so each takes
// chunks the previous one released. Every count must be exact.
func TestCountRows_PooledChunksAcrossStatements(t *testing.T) {
	for _, n := range []int{9000, 3, DefaultChunkCapacity, 0, 10_001, 1, DefaultChunkCapacity + 1, 64} {
		if got, want := countEvens(t, n), int64((n+1)/2); got != want {
			t.Fatalf("n=%d: count %d, want %d", n, got, want)
		}
	}
}

// TestColumnarFilter_CloseReleasesScratchWithoutAliasing holds a filter's output
// chunk (a pooled shape) as a retaining consumer would, closes the filter, then
// runs further statements that draw from the pool. The held rows must be intact:
// the filter released only its own scratch, never the consumer's chunk. The same
// filter, re-initialised after Close, must produce the same rows again.
func TestColumnarFilter_CloseReleasesScratchWithoutAliasing(t *testing.T) {
	const n = 9000
	cf := NewColumnarFilter(NewAllNodesScan(poolWalker(n)), poolKeepEvenBoxed, poolKeepEven)
	drain := func() *Chunk {
		if err := cf.Init(context.Background()); err != nil {
			t.Fatalf("Init: %v", err)
		}
		held := cf.NewOutputChunk(DefaultChunkCapacity)
		for {
			got, err := cf.FillChunk(held, DefaultChunkCapacity)
			if err != nil {
				t.Fatalf("FillChunk: %v", err)
			}
			if got < DefaultChunkCapacity {
				break
			}
		}
		if err := cf.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if cf.scratch != nil {
			t.Fatal("Close kept the scratch chunk")
		}
		return held
	}
	check := func(held *Chunk) {
		t.Helper()
		if held.Len() != (n+1)/2 {
			t.Fatalf("held %d rows, want %d", held.Len(), (n+1)/2)
		}
		for r := 0; r < held.Len(); r++ {
			if v, valid := held.Int64(0, r); !valid || v != int64(2*r) {
				t.Fatalf("held row %d = (%d, %v), want %d", r, v, valid, 2*r)
			}
		}
	}
	first := drain()
	for _, m := range []int{5000, 7, 12_000} {
		countEvens(t, m) // reuses whatever the pool holds
	}
	check(first)
	second := drain() // re-initialised after Close
	check(second)
	check(first)
}
