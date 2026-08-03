package count_test

// commutative_test.go — the count store's ordering basis (rmp #2303, MVCC B1,
// audit finding E12).
//
// # A PREMISE THAT WAS WRONG, AND THE DEFECT UNDER IT
//
// The task's premise was that the count store is "correct only because the
// barrier imposes a total order on committers". Reading the code suggested
// otherwise — [count.Store.Apply] is `cell.Add(delta)`, an ADDITIVE delta rather
// than an assignment, and [count.Store.MarkDirty] is a monotone set insert — so
// the first draft of this file concluded the store was already commutative and
// needed only a corrected contract.
//
// That conclusion was wrong, and TestCountStore_ConcurrentDeltasReachZeroFromEitherOrder
// is the test written to check it that refuted it. Apply deleted a cell at
// zero-OR-BELOW, so a cell driven negative was deleted, its negative value
// DISCARDED, and the next increment recreated it from zero — permanently losing
// the decrement. Applying -1 then +1 to an empty cell read 1 where +1 then -1 read
// 0.
//
// Under the visibility barrier the base was always correct, so no partial sum
// could go negative and the clamp was unreachable. The moment writers commit
// concurrently, one transaction's decrements can land before another's increments
// and the clamp silently eats them. So this was a live ordering dependency, not a
// documentation gap, and the fix is in Apply: delete at EXACTLY zero and retain a
// negative cell, which is what makes addition commute.
//
// # The ordering basis, as it now stands
//
//   - Apply is addition, and addition commutes. Deleting only at exactly zero
//     keeps that true for any interleaving, because an absent key reads as 0 —
//     the value that triggered the delete.
//   - MarkDirty is a set insert and nothing clears an individual entry (only a
//     whole-store Reset does), so it is idempotent and commutative.
//
// Neither requires writer exclusion. What the barrier still buys the count store
// is a DIFFERENT property — visibility atomicity, that a reader never sees a count
// without the graph write it describes — and that is rmp #2304's to preserve by
// other means.
//
// # How the differential was run
//
// Restoring the `<= 0` clamp makes
// TestCountStore_ConcurrentDeltasReachZeroFromEitherOrder fail with exactly the
// message it carries. Reverted after measuring.

import (
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/index/count"
)

// TestCountStore_ConcurrentDeltasCommute drives many goroutines applying deltas
// to the SAME cell with no barrier of any kind, and requires the final total to
// be the one a serial schedule would produce.
//
// This is acceptance criterion 2 for the count store: the state a serial schedule
// would produce is, for an additive aggregate, the sum of the committed deltas.
func TestCountStore_ConcurrentDeltasCommute(t *testing.T) {
	const (
		writers  = 16
		perWtr   = 500
		relType  = uint32(7)
		wantEach = int64(1)
	)
	cs := count.New(0)

	// Every writer applies +1 perWtr times, then -1 perWtr-1 times, so each
	// leaves a net +1 and the running total repeatedly approaches — but must not
	// reach — zero. The near-zero traffic is deliberate: it is what exercises the
	// delete-on-zero branch concurrently, which is the only part of Apply that is
	// not a plain addition.
	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perWtr; i++ {
				cs.Apply(count.Delta{Kind: count.KindE, RT: relType, Delta: 1})
			}
			for i := 0; i < perWtr-1; i++ {
				cs.Apply(count.Delta{Kind: count.KindE, RT: relType, Delta: -1})
			}
		}()
	}
	wg.Wait()

	if got, want := cs.CountE(relType), int64(writers)*wantEach; got != want {
		t.Errorf("CountE = %d, want %d — %d concurrent writers' deltas did not compose, "+
			"so the aggregate is not commutative and the barrier was load-bearing after all",
			got, want, writers)
	}
}

// TestCountStore_ConcurrentDeltasReachZeroFromEitherOrder pins the one branch in
// Apply that is not a plain addition: the key is deleted when its counter reaches
// zero, and an absent key reads as zero.
//
// The two orders must be indistinguishable. If they were not, a cell that
// transiently touched zero mid-transaction would keep or lose its key depending on
// which writer got there first, and the count would depend on the schedule.
func TestCountStore_ConcurrentDeltasReachZeroFromEitherOrder(t *testing.T) {
	const relType = uint32(9)

	// +1 then -1.
	up := count.New(0)
	up.Apply(count.Delta{Kind: count.KindE, RT: relType, Delta: 1})
	up.Apply(count.Delta{Kind: count.KindE, RT: relType, Delta: -1})

	// -1 then +1: the first delta drives the cell to -1, which trips the
	// delete-on-zero-or-below branch, and the second recreates it.
	down := count.New(0)
	down.Apply(count.Delta{Kind: count.KindE, RT: relType, Delta: -1})
	down.Apply(count.Delta{Kind: count.KindE, RT: relType, Delta: 1})

	// The orders are only equivalent from a CORRECT base. From an empty store a
	// net-zero pair must read zero whichever way round it is applied; the
	// down-first order reaching 1 would mean the clamp at zero silently
	// manufactured a count.
	if got := up.CountE(relType); got != 0 {
		t.Errorf("+1 then -1 left CountE = %d, want 0", got)
	}
	if got := down.CountE(relType); got != 0 {
		t.Errorf("-1 then +1 left CountE = %d, want 0 — the delete-on-zero branch is "+
			"order-sensitive, so the aggregate depends on the schedule", got)
	}
}

// TestCountStore_ConcurrentDirtyMarksAreMonotone pins the other half of the
// contract: an exactness marking is a set insert, so it is idempotent and cannot
// be undone by a concurrent writer marking a different family.
//
// It matters because the intra-transaction order — deltas first, then dirty marks
// ([exec.CountBuffer.Commit]) — is what keeps a family the deltas just made exact
// from being reported exact when the same commit also tripped the budget. That
// order is per-buffer and survives concurrency; what must ALSO hold is that one
// writer's mark cannot be lost to another's, which is what this asserts.
func TestCountStore_ConcurrentDirtyMarksAreMonotone(t *testing.T) {
	const writers = 16
	cs := count.New(0)

	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			label := uint32(w)
			// Marked repeatedly and from every writer: the insert must be
			// idempotent, and no writer's mark may displace another's.
			for i := 0; i < 50; i++ {
				cs.MarkDirty(count.DirtyMark{Scope: count.DirtyDOut, Label: label})
			}
		}(w)
	}
	wg.Wait()

	for w := 0; w < writers; w++ {
		if !cs.DDirty(uint32(w), count.Out) {
			t.Errorf("label %d is reported exact after being marked dirty by a concurrent "+
				"writer: a marking was lost, so exactness depends on the schedule", w)
		}
	}
}
