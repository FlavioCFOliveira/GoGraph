package lpg

// mvcc_commit_ts_test.go — rmp #2309 (MVCC C3a): allocating a commit timestamp
// before the durability point, and discharging it on every path out.
//
// Layer: short.
//
// The allocation moved ahead of the WAL fsync so the instant can be written INTO the
// durable record. That creates one obligation and one hazard, and both are here: an
// allocated timestamp must always be published or abandoned, and the recycled
// per-transaction state must never carry one across into the next transaction.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// commitTSGraph builds a versioned graph, which is the only configuration in which a
// commit timestamp exists at all.
func commitTSGraph(t *testing.T) *Graph[string, float64] {
	t.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.armMVCC()
	return g
}

// TestAllocateCommitTS_IsIdempotent pins that two callers on one transaction get one
// instant. Burning a second would hold the contiguous frontier back for no reason,
// and — worse — the record would publish one of them while the WAL carried the other.
func TestAllocateCommitTS_IsIdempotent(t *testing.T) {
	g := commitTSGraph(t)
	var first, second uint64
	if err := g.ApplyVersioned(func(tx WriteTx) error {
		if err := g.Writer(tx).AddNode("a"); err != nil {
			return err
		}
		first = g.AllocateCommitTS(tx)
		second = g.AllocateCommitTS(tx)
		return nil
	}); err != nil {
		t.Fatalf("ApplyVersioned: %v", err)
	}
	if first == 0 {
		t.Fatal("AllocateCommitTS returned 0 for an armed graph inside a transaction: a " +
			"durable writer would encode 'no timestamp' and recovery could not derive " +
			"the clock")
	}
	if second != first {
		t.Fatalf("two calls returned %d and %d: a second allocation is a timestamp that "+
			"will never be published, and one that is neither published nor abandoned "+
			"stalls the contiguous frontier permanently", first, second)
	}
}

// TestAllocateCommitTS_IsTheTimestampActuallyPublished is the property the whole
// change exists for: what a durable record would carry must be what becomes visible.
// If the two diverged, the derived clock floor would sit somewhere the graph never
// actually was.
func TestAllocateCommitTS_IsTheTimestampActuallyPublished(t *testing.T) {
	g := commitTSGraph(t)
	var allocated uint64
	if err := g.ApplyVersioned(func(tx WriteTx) error {
		if err := g.Writer(tx).AddNode("a"); err != nil {
			return err
		}
		allocated = g.AllocateCommitTS(tx)
		return nil
	}); err != nil {
		t.Fatalf("ApplyVersioned: %v", err)
	}
	if got := g.MVCCStats().Now; got != allocated {
		t.Fatalf("the transaction allocated instant %d but the clock published %d: the "+
			"durable record and the visible instant disagree, so recovery would derive a "+
			"floor that does not match what readers saw", allocated, got)
	}
	if n := g.MVCCStats().InFlightCommits; n != 0 {
		t.Fatalf("InFlightCommits = %d after the transaction finished, want 0", n)
	}
}

// TestAllocateCommitTS_UnpublishedAllocationIsAbandoned covers the versioned-nothing
// exit. A durable caller allocates before it knows whether anything was versioned, so
// this path routinely holds a timestamp with no commit record to publish it into.
func TestAllocateCommitTS_UnpublishedAllocationIsAbandoned(t *testing.T) {
	g := commitTSGraph(t)
	for i := 0; i < 8; i++ {
		if err := g.ApplyVersioned(func(tx WriteTx) error {
			// Version NOTHING, then allocate anyway — exactly what a durable commit
			// path does, since it allocates before the fsync and cannot know yet.
			g.AllocateCommitTS(tx)
			return nil
		}); err != nil {
			t.Fatalf("ApplyVersioned %d: %v", i, err)
		}
	}
	if n := g.MVCCStats().InFlightCommits; n != 0 {
		t.Fatalf("InFlightCommits = %d after eight transactions that versioned nothing, "+
			"want 0: each allocated an instant that was never published, and one that is "+
			"never abandoned either stalls the contiguous frontier FOREVER — every later "+
			"commit becomes invisible to new readers", n)
	}
	// The frontier must still be usable: a commit after the abandoned run has to
	// become visible.
	if err := g.ApplyVersioned(func(tx WriteTx) error {
		return g.Writer(tx).AddNode("a")
	}); err != nil {
		t.Fatalf("ApplyVersioned after the abandoned run: %v", err)
	}
	if g.MVCCStats().Now == 0 {
		t.Fatal("the visible instant is still 0 after a successful commit: the frontier " +
			"is stalled behind an abandoned allocation that was never discharged")
	}
}

// TestAllocateCommitTS_RecycledStateCarriesNoStaleTimestamp is the regression named in
// acquireWriteCtx.
//
// Per-transaction state is pooled. A commitTS left behind on it would be published by
// the NEXT transaction, so two transactions would share one instant and the second's
// writes would become visible at the first's — a reader between them sees a state
// neither transaction ever produced.
func TestAllocateCommitTS_RecycledStateCarriesNoStaleTimestamp(t *testing.T) {
	g := commitTSGraph(t)

	// A transaction that allocates and publishes, so its state goes back to the free
	// list carrying a used timestamp if it is not cleared.
	var first uint64
	if err := g.ApplyVersioned(func(tx WriteTx) error {
		if err := g.Writer(tx).AddNode("a"); err != nil {
			return err
		}
		first = g.AllocateCommitTS(tx)
		return nil
	}); err != nil {
		t.Fatalf("first: %v", err)
	}

	// The next transaction very likely reuses that state. It must mint its own.
	var second uint64
	if err := g.ApplyVersioned(func(tx WriteTx) error {
		if err := g.Writer(tx).AddNode("b"); err != nil {
			return err
		}
		second = g.AllocateCommitTS(tx)
		return nil
	}); err != nil {
		t.Fatalf("second: %v", err)
	}

	if second == first {
		t.Fatalf("two consecutive transactions both allocated instant %d: recycled "+
			"per-transaction state carried a stale commit timestamp, so the second "+
			"published at the first's instant and a reader between them sees a state "+
			"neither produced", first)
	}
	if second < first {
		t.Fatalf("the second transaction allocated %d, before the first's %d: the clock "+
			"is not monotonic", second, first)
	}
	if n := g.MVCCStats().InFlightCommits; n != 0 {
		t.Fatalf("InFlightCommits = %d, want 0", n)
	}
}

// TestAllocateCommitTS_ZeroWhenThereIsNothingToStamp pins the two "no timestamp"
// answers, so a durable caller can invoke it unconditionally.
func TestAllocateCommitTS_ZeroWhenThereIsNothingToStamp(t *testing.T) {
	// Disarmed graph: no MVCC clock, so no instant.
	disarmed := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if got := disarmed.AllocateCommitTS(WriteTx{}); got != 0 {
		t.Fatalf("AllocateCommitTS on a disarmed graph = %d, want 0", got)
	}
	// Armed graph, zero transaction.
	if got := commitTSGraph(t).AllocateCommitTS(WriteTx{}); got != 0 {
		t.Fatalf("AllocateCommitTS with no transaction = %d, want 0", got)
	}
}
