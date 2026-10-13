package adjlist

// direct_conflict_test.go — rmp #2947 at the layer that owns the adjacency: a
// write carrying no transaction refuses to publish over an uncommitted entry.

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// entryOf returns the current entry pointer of key, the identity a refused
// write must leave untouched.
func entryOf(a *AdjList[string, float64], key string) *adjEntry[float64] {
	id, _ := a.Mapper().Lookup(key)
	return loadEntry[float64](&a.shards[id&shardMask], uint64(id)>>shardBits)
}

func requireAdjConflict(t *testing.T, err error, op string) {
	t.Helper()
	var c *mvcc.Conflict
	if !errors.As(err, &c) || c.Store != mvcc.StoreAdjacency || !c.ConcurrentWriter() {
		t.Fatalf("%s over an uncommitted entry returned %v, want an in-flight adjacency *mvcc.Conflict", op, err)
	}
}

// TestDirectConflict_UntransactedWriteNeverAdoptsTheSlot is rmp #2967 at the
// layer that owns the adjacency. A write carrying no transaction used to adopt
// the transaction the write stamp's slot named — an exclusive bracket of the
// layer above — as its own: it displaced that transaction's uncommitted entry
// and took its commit record, so the bracket's abort took an acknowledged write
// with it. It is now its own single-operation transaction: refused over the
// slot transaction's uncommitted entry, and committed at once under its own
// timestamp elsewhere, so the slot transaction's abort leaves it visible. The
// slot transaction's own writes, carried through a [Writer], are admitted over
// its own entry as before.
func TestDirectConflict_UntransactedWriteNeverAdoptsTheSlot(t *testing.T) {
	a := New[string, float64](Config{})
	a.EnableVersioning()
	clk := &mvcc.Clock{}
	ws := &mvcc.WriteStamp{}
	ws.SetClock(clk)
	a.SetWriteStamp(ws)
	for _, n := range []string{"x", "y", "z"} {
		if err := a.AddNode(n); err != nil {
			t.Fatal(err)
		}
	}
	tx := beginTxW(ws)
	wr := a.Writer(tx)
	if err := wr.AddEdge("x", "y", 1); err != nil {
		t.Fatalf("first write of the slot transaction: %v", err)
	}
	if err := wr.AddEdge("x", "z", 1); err != nil {
		t.Fatalf("a second write of the SAME transaction was refused against its own version: %v", err)
	}
	before := entryOf(a, "x")
	requireAdjConflict(t, a.AddEdge("x", "z", 2), "an untransacted AddEdge over the slot transaction's entry")
	requireAdjConflict(t, a.RemoveEdge("x", "y"), "an untransacted RemoveEdge over the slot transaction's entry")
	if got := entryOf(a, "x"); got != before {
		t.Fatal("a refused untransacted write replaced the slot transaction's entry")
	}
	if err := a.AddEdge("y", "z", 3); err != nil {
		t.Fatalf("an untransacted write over a committed entry: %v", err)
	}
	afterRaw := clk.ReadTS()
	info, _ := ws.End()
	if info == nil {
		t.Fatal("the slot transaction holds no record")
	}
	info.Abort()
	if got := a.EntryNeighboursAsOf(idOf(t, a, "y"), afterRaw, 0); len(got) != 1 || got[0] != idOf(t, a, "z") {
		t.Fatalf("after the slot transaction aborted, y's committed neighbours are %v, want [z]: "+
			"the untransacted write joined the aborted transaction", got)
	}
	if got := a.EntryNeighboursAsOf(idOf(t, a, "x"), clk.ReadTS(), 0); len(got) != 0 {
		t.Fatalf("x's committed neighbours are %v after the abort, want none", got)
	}
}

// LoadEntryHandles is a test convenience over the handle column of key's entry.
func (a *AdjList[N, W]) LoadEntryHandles(key N) ([]graph.NodeID, []uint64) {
	id, ok := a.Mapper().Lookup(key)
	if !ok {
		return nil, nil
	}
	nbs, _, hs := a.LoadEntryH(id)
	return nbs, hs
}
