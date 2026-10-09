package lpg

// abort_window_existence_test.go — regression gate for rmp #3029.
//
// Withdrawing an aborted first creation marks the id unborn under the life-shard
// lock and flips its tombstone only afterwards (finishLifeWithdrawal). The write
// paths that may create a node — internEndpoint (AddEdge and AddEdgeH endpoints,
// SetNodeLabel, SetNodeProperty) and addNodeInfo's fast path — classify the key in
// two reads: "unborn and tombstoned" (revive it) and then existenceNoOpAdmits,
// which refused only "unborn and NOT tombstoned". A write whose first read fell
// before the flip and whose second fell after it passed both: it built on a node
// that was now dead without recreating it, so the commit marker's id annex did
// not name the node and recovery refused the log with ErrUnboundNodeKey
// (TestDifferential_MemoryEqualsRecovery, 2 to 3 runs in 20).
//
// The second read sees the post-flip state, which is stable once the withdrawal
// is over, so the race is pinned deterministically by asking existenceNoOpAdmits
// about an unborn, tombstoned id: a no-op existence verdict must never be taken
// on an unborn id, whatever its tombstone shows.
//
// Layer: short.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

func TestExistenceNoOpAdmits_RefusesAnUnbornID(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	abortedDurable(t, g, func(w WriteView[string, float64]) error { return w.AddNode("x") })
	id, _ := g.adj.Mapper().Lookup("x")
	if !g.IsTombstonedStored(id) || !g.inUnborn(id) {
		t.Fatalf("premise: x must be dead and unborn after its creation aborted (dead=%v unborn=%v)",
			g.IsTombstonedStored(id), g.inUnborn(id))
	}
	var admitted bool
	err := g.ApplyDurable(context.Background(), func(wtx WriteTx) error {
		admitted = g.existenceNoOpAdmits(id, wtx.w)
		return wtx.w.err()
	}, func() error { return nil })
	if admitted {
		t.Fatal("a write took the no-op existence verdict on an unborn id: it would build on a node it does " +
			"not recreate, and the commit annex would not name it")
	}
	if !errors.Is(err, mvcc.ErrSerializationConflict) {
		t.Fatalf("ApplyDurable = %v, want a retryable serialization conflict", err)
	}
}

// TestRemoveNode_RefusesAnUnbornID pins the removal side of the same window. In
// it the withdrawn node is unborn, has no record, and still looks alive, so a
// removal recorded the death of a living node; when that removal aborted, the
// withdrawal of a lone death REVIVED the node and cleared its unborn mark,
// leaving a node no committed transaction created, which later writes built on
// without the commit annex naming it. The test reproduces what the removal sees
// in the window — a live node marked unborn — and requires the death to be
// refused, so nothing is recorded that an abort could turn into a revival.
func TestRemoveNode_RefusesAnUnbornID(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	committedDurable(t, g, func(w WriteView[string, float64]) error { return w.AddNode("x") })
	id, _ := g.adj.Mapper().Lookup("x")
	g.markUnborn(id) // the instant between markUnborn and the tombstone flip
	err := g.ApplyDurable(context.Background(), func(wtx WriteTx) error {
		_, _ = g.Writer(wtx).RemoveNode("x")
		return wtx.w.err()
	}, func() error { return nil })
	if !errors.Is(err, mvcc.ErrSerializationConflict) {
		t.Fatalf("ApplyDurable = %v, want a retryable serialization conflict: a death was recorded on an unborn id", err)
	}
	if g.IsTombstonedStored(id) {
		t.Fatal("the refused removal tombstoned the node")
	}
}
