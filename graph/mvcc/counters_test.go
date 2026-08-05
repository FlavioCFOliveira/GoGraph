package mvcc

// counters_test.go — rmp #2312: what a striped counter bank must guarantee, and the
// one thing it deliberately does not.

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestWriteCounters_ExactWhenQuiescent asserts the sum is EXACT once every writer has
// finished, which is the property striping is allowed to cost nothing of.
//
// The counts are spread over cache-line-isolated banks and summed one at a time, so a
// sum taken mid-flight may miss a transaction. A sum taken when nothing is in flight
// may not: a telemetry series that loses commits permanently is not a series.
func TestWriteCounters_ExactWhenQuiescent(t *testing.T) {
	var c WriteCounters
	const goroutines, per = 8, 500
	var next atomic.Uint64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				// Monotonic ids, exactly as the clock mints them, so the stripe
				// selection under test is the one production uses.
				id := TxIDBase + next.Add(1)
				c.BeginWriter(id)
				if i%3 == 0 {
					c.Abort(id)
				} else {
					c.Commit(id)
				}
				c.EndWriter(id)
			}
		}()
	}
	wg.Wait()

	got := c.Load()
	const total = goroutines * per
	wantAborts := uint64(0)
	for i := 0; i < per; i++ {
		if i%3 == 0 {
			wantAborts++
		}
	}
	wantAborts *= goroutines
	if got.Commits+got.Aborts != total {
		t.Errorf("commits+aborts = %d, want %d: the striped sum lost transactions",
			got.Commits+got.Aborts, total)
	}
	if got.Aborts != wantAborts {
		t.Errorf("aborts = %d, want %d", got.Aborts, wantAborts)
	}
	if got.Writers != 0 {
		t.Errorf("writers = %d after every transaction closed, want 0: a begin and an end "+
			"carrying the same id must cancel on the same stripe", got.Writers)
	}
}

// TestWriteCounters_WritersNeverNegative asserts the gauge cannot read below zero even
// while transactions are opening and closing on many goroutines.
//
// This is the property that justifies striping a GAUGE at all: a signed per-stripe
// counter would be free to go negative if a transaction's begin and end could land on
// different stripes, and a negative writer count is a number an operator cannot
// interpret. Both calls take the transaction's own id, so they cannot.
func TestWriteCounters_WritersNeverNegative(t *testing.T) {
	var c WriteCounters
	var next atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				id := TxIDBase + next.Add(1)
				c.BeginWriter(id)
				c.Commit(id)
				c.EndWriter(id)
			}
		}()
	}
	// Sampled while the writers run, which is the only regime in which the invariant
	// can be broken.
	sawPositive := false
	for i := 0; i < 20000; i++ {
		if n := c.Writers(); n < 0 {
			close(stop)
			wg.Wait()
			t.Fatalf("the writer gauge read %d", n)
		} else if n > 0 {
			sawPositive = true
		}
	}
	close(stop)
	wg.Wait()
	// ASSERT SOMETHING WAS SEEN: a gauge that only ever read zero would satisfy the
	// non-negative check above without the counter working at all.
	if !sawPositive {
		t.Error("the writer gauge never read above zero while four goroutines held " +
			"transactions open: the sampling never overlapped a bracket, so this test " +
			"proved nothing about the invariant it exists to check")
	}
	if n := c.Writers(); n != 0 {
		t.Errorf("the writer gauge is %d after every writer stopped, want 0", n)
	}
}

// TestWriteCounters_ConflictAttribution asserts a conflict lands in its own bucket and
// in the total, and that the rate divides by the transactions that reached a decision.
func TestWriteCounters_ConflictAttribution(t *testing.T) {
	var c WriteCounters
	labels := ConflictStoreIndex(StoreNodeLabels)
	adj := ConflictStoreIndex(StoreAdjacency)
	// Three doomed transactions. Each is counted once as a conflict — the CAUSE — and
	// once as the abort it became — the OUTCOME. The two are not a partition of each
	// other, and mixing them up is what makes a rate wrong: see [WriteCounts].
	for _, s := range []int{labels, labels, adj} {
		c.Conflict(s)
		c.Abort(TxIDBase + uint64(s))
	}
	c.Commit(TxIDBase + 1)

	got := c.Load()
	if got.Conflicts != 3 {
		t.Errorf("conflicts = %d, want 3", got.Conflicts)
	}
	if got.ByStore[labels] != 2 || got.ByStore[adj] != 1 {
		t.Errorf("per-store conflicts = labels:%d adjacency:%d, want 2 and 1",
			got.ByStore[labels], got.ByStore[adj])
	}
	// 3 conflicts over 1 commit + 3 aborts = 3/4. The denominator is the OUTCOMES,
	// and a rate above 1 is the symptom of adding the cause to them.
	if rate := got.ConflictRate(); rate != 0.75 {
		t.Errorf("ConflictRate() = %v, want 0.75", rate)
	}
	if rate := got.ConflictRate(); rate > 1 {
		t.Errorf("ConflictRate() = %v, which cannot exceed 1: a conflicting transaction is "+
			"being counted outside the denominator it belongs in", rate)
	}
	var empty WriteCounts
	if rate := empty.ConflictRate(); rate != 0 {
		t.Errorf("ConflictRate() on an idle graph = %v, want 0 rather than a division by zero", rate)
	}
}

// TestWriteCounters_StripesAreDistinct asserts that consecutive transaction ids do not
// share a bank, which is the whole reason the id is the stripe key.
//
// If they did, N concurrent writers would contend on one cache line and the striping
// would be pure cost. It is checked through the public API rather than by reading the
// index, because the index is an implementation detail and the property is not.
func TestWriteCounters_StripesAreDistinct(t *testing.T) {
	var c WriteCounters
	for i := 0; i < counterStripes; i++ {
		c.Commit(TxIDBase + uint64(i))
	}
	for i := 0; i < counterStripes; i++ {
		if got := c.stripes[i].commits.Load(); got != 1 {
			t.Fatalf("stripe %d holds %d commits after one commit per stripe, want 1: "+
				"consecutive transaction ids are colliding", i, got)
		}
	}
}
