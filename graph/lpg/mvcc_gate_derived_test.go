package lpg

// mvcc_gate_derived_test.go — the constraintActive/indexActive gates' ordering
// basis (rmp #2303, MVCC B1, last of the task's four structures).
//
// # The dependency that was there
//
// Both gates were an atomic.Int64 that the cypher engine STORED a separately-read
// registry count into (Engine.syncConstraintCount / syncIndexCount), documented as
// correct because the engine held its single-writer lock. That documentation was
// accurate and it is exactly the kind of dependency this task exists to remove: a
// store of a value the caller read earlier is a lost update the moment a second
// writer exists.
//
// The interleaving that matters, with one index registered:
//
//	A drops it        → registry 0, A reads 0
//	B creates another → registry 1, B reads 1, B stores 1
//	A stores 0        → GATE 0, REGISTRY 1
//
// The gate now UNDER-REPORTS, and under-reporting is the dangerous direction: the
// checkpointer's phase-3 self-sufficiency re-check consults HasIndexes to decide
// whether the WAL prefix holding a CREATE INDEX may be truncated. A false answer
// truncates it and the index is silently gone on the next reopen — which is
// precisely the defect #1755 closed, resurrected by concurrency.
//
// # The basis now, which is the task's own preferred answer
//
// DERIVED, not maintained. The graph holds a function and calls it at the instant
// the question is asked, so there is no stored value to go stale, no window, and no
// ordering requirement on the caller whatsoever. The task's technical requirements
// asked for exactly this — "derived rather than maintained, or maintained under a
// rule that survives concurrency" — and deriving is strictly the stronger of the
// two: it removes the race instead of guarding it, and adds no lock.

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestDerivedGate_TracksItsSourceWithNoNotification is the discriminating test,
// and the property it asserts is the one that removes the ordering requirement:
// the gate reflects the source's CURRENT value, with nothing having told it the
// source changed.
//
// A mirrored gate cannot pass this. Its value is whatever was last stored, so
// until someone calls the setter it reports the stale count — which is what made
// the old design need a lock, and what a lost update then defeated anyway.
func TestDerivedGate_TracksItsSourceWithNoNotification(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})

	var indexes, constraints atomic.Int64
	g.SetIndexCountSource(indexes.Load)
	g.SetConstraintCountSource(constraints.Load)

	if g.HasIndexes() || g.HasConstraints() {
		t.Fatal("an empty registry must not report indexes or constraints")
	}

	// Change the source only. Nothing notifies the graph.
	indexes.Store(1)
	if !g.HasIndexes() {
		t.Error("HasIndexes is false with 1 index in the source and no notification: the " +
			"gate is a stored mirror, so it is stale until someone remembers to update it")
	}
	constraints.Store(1)
	if !g.HasConstraints() {
		t.Error("HasConstraints is false with 1 constraint in the source and no notification")
	}

	// And back down, because over-reporting forever would also be wrong: the
	// checkpointer would retain WAL prefixes it never needs.
	indexes.Store(0)
	constraints.Store(0)
	if g.HasIndexes() || g.HasConstraints() {
		t.Error("the gates still report after their sources emptied: derived means derived " +
			"in both directions")
	}
}

// TestDerivedGate_NeverUnderReportsUnderConcurrentChurn drives the interleaving
// that broke the mirrored design: concurrent create/drop churn against one
// registry, with the gate sampled throughout.
//
// The invariant is one-directional on purpose. Over-reporting is safe — the
// checkpointer merely retains a WAL prefix it did not need — while under-reporting
// truncates a prefix holding a live definition and loses it. So the assertion is:
// whenever the source is non-empty at the moment the gate is read, the gate must
// say so.
//
// A derived gate satisfies this by construction, which is the point: there is no
// interval between the source changing and the gate agreeing.
func TestDerivedGate_NeverUnderReportsUnderConcurrentChurn(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})

	// The registry stays >= 1 throughout, so the gate must NEVER report false.
	// Churn moves it between 1 and 2 so a stale store has something to be stale
	// about.
	var registry atomic.Int64
	registry.Store(1)
	g.SetIndexCountSource(registry.Load)

	const rounds = 2000
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Two churners, so a lost update has two writers to be lost between.
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				registry.Store(2)
				registry.Store(1)
			}
		}()
	}

	// The reader: the checkpointer's question, asked repeatedly.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < rounds; i++ {
			if !g.HasIndexes() {
				t.Errorf("HasIndexes reported FALSE on round %d while the registry never "+
					"dropped below 1: the gate under-reports, so the checkpointer would "+
					"truncate the WAL prefix holding a live CREATE INDEX and lose it", i)
				return
			}
		}
	}()
	wg.Wait()
}

// TestDerivedGate_DetachedSourceFallsBackToStoreDirect pins the nil case.
//
// An embedder driving txn.Store directly attaches no engine source, and its
// definitions are counted by the store-direct counters instead. The gate must
// answer from those rather than reporting nothing.
func TestDerivedGate_DetachedSourceFallsBackToStoreDirect(t *testing.T) {
	g := New[string, int64](adjlist.Config{Directed: true})

	// No source attached at all.
	if g.HasIndexes() || g.HasConstraints() {
		t.Fatal("a graph with no engine source and no store-direct definitions must report neither")
	}

	// Attach and detach: detaching must not leave a stale true behind.
	var n atomic.Int64
	n.Store(1)
	g.SetIndexCountSource(n.Load)
	if !g.HasIndexes() {
		t.Fatal("HasIndexes is false with an attached non-empty source")
	}
	g.SetIndexCountSource(nil)
	if g.HasIndexes() {
		t.Error("HasIndexes still reports after its source was detached: the gate kept a " +
			"stale value, which is the stored-mirror behaviour this change removes")
	}
}
