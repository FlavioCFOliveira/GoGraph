package mvcc

// stamp_test.go — the per-transaction stamping state (rmp #2301, audit finding
// E3).
//
// Every test here is about ONE claim: two write transactions open at the same
// time must not be able to lose each other's commit record. Before rmp #2301 the
// record, the version count and the pending transaction id were fields on the
// WriteStamp, one set per graph, and the second Begin overwrote the first.
//
// That failure is NOT a data race — every field was atomic, so -race is silent
// on it. It is silent data loss, and TestWriteStamp_TwoTransactionsDoNotShareState
// is what measures it. Against the pre-change build the same scenario left the
// first transaction's record unpublished and unreachable: its versions kept a
// transaction id forever, so no reader could ever see them and no reclaimer could
// ever free them.

import (
	"sync"
	"testing"
)

// armAndPublish opens a window on ws for a fresh transaction: arm the state, then
// name it. The two are separate so neither has an ignorable failure mode — see
// [WriteStamp.Publish].
func armAndPublish(ws *WriteStamp, st *TxState, txID uint64) {
	if !st.Arm(txID) {
		panic("mvcc: test armed a state that still holds a record")
	}
	ws.Publish(st)
}

// TestWriteStamp_TwoTransactionsDoNotShareState is the E3 regression.
//
// Two transactions arm, both version something, and both close. Each must get
// back its OWN record with its OWN version count. Against the per-graph shape the
// second Arm destroyed the first transaction's window, so its record was never
// returned by anything and never published.
func TestWriteStamp_TwoTransactionsDoNotShareState(t *testing.T) {
	t.Parallel()
	var ws WriteStamp
	var clk Clock
	ws.SetClock(&clk)

	var stA, stB TxState
	idA, idB := clk.NextTxID(), clk.NextTxID()
	if !stA.Arm(idA) {
		t.Fatal("Arm(A) refused a fresh state")
	}
	ws.Publish(&stA)
	recA := stA.Ensure()
	if recA == nil {
		t.Fatal("A's first version got no record")
	}

	// B opens while A is still open — the situation the barrier used to make
	// impossible and rmp #2304 will make ordinary.
	if !stB.Arm(idB) {
		t.Fatal("Arm(B) refused a fresh state")
	}
	ws.Publish(&stB)
	recB := stB.Ensure()
	if recB == nil {
		t.Fatal("B's first version got no record")
	}
	if recA == recB {
		t.Fatal("two open transactions share ONE commit record: publishing either publishes both")
	}
	if recA.TS() != idA || recB.TS() != idB {
		t.Fatalf("records carry ids (%d, %d), want (%d, %d): a record built for the wrong "+
			"transaction is invisible to its own writer",
			recA.TS(), recB.TS(), idA, idB)
	}

	// A closes second, out of order, and must still get its own record back.
	gotB, nB := ws.End()
	gotA, nA := stA.Retract()
	if gotB != recB || nB != 1 {
		t.Fatalf("B retracted (%v, %d), want (%v, 1)", gotB, nB, recB)
	}
	if gotA != recA || nA != 1 {
		t.Fatalf("A retracted (%v, %d), want (%v, 1) — the per-graph shape returned nil here, "+
			"leaving A's versions stamped with an id no reader can ever pass", gotA, nA, recA)
	}
}

// TestWriteStamp_VersionCountIsPerTransaction is the accounting half of the same
// property: the count charged to the reclamation budget must be the count of the
// transaction being closed, not of whatever else was open.
func TestWriteStamp_VersionCountIsPerTransaction(t *testing.T) {
	t.Parallel()
	var ws WriteStamp
	var clk Clock
	ws.SetClock(&clk)

	var stA, stB TxState
	armAndPublish(&ws, &stA, clk.NextTxID())
	for i := 0; i < 3; i++ {
		stA.Ensure()
	}
	armAndPublish(&ws, &stB, clk.NextTxID())
	for i := 0; i < 7; i++ {
		stB.Ensure()
	}

	if _, n := ws.End(); n != 7 {
		t.Fatalf("B closed with %d versions, want 7", n)
	}
	if _, n := stA.Retract(); n != 3 {
		t.Fatalf("A closed with %d versions, want 3: a shared counter reports one "+
			"transaction's churn as another's", n)
	}
}

// TestWriteStamp_RetractedWindowRefusesLateVersions pins the direction of the
// unsafe failure, which recycling introduces and which the sentinel closes.
//
// A version that arrives after its transaction was retracted must NOT be stamped
// with that transaction's record: the record already carries a commit timestamp,
// so the version would become visible to readers whose snapshot predates the
// write. Later-than-it-happened is safe; earlier is a stale snapshot observing a
// future write.
func TestWriteStamp_RetractedWindowRefusesLateVersions(t *testing.T) {
	t.Parallel()
	var ws WriteStamp
	var clk Clock
	ws.SetClock(&clk)

	var st TxState
	armAndPublish(&ws, &st, clk.NextTxID())
	st.Ensure()
	info, _ := ws.End()
	info.Commit(clk.NextCommitTS())

	if got := st.Ensure(); got != nil {
		t.Fatalf("a retracted transaction handed out record %v; a version stamped with an "+
			"already-committed record becomes visible before it happened", got)
	}
	// And the stamp routes such a write to a timestamp of its own instead.
	rec, ts := ws.Stamp()
	if rec != nil || ts == 0 {
		t.Fatalf("Stamp with no window returned (%v, %d), want (nil, a fresh timestamp)", rec, ts)
	}
	if ws.TakeUntracked() != 1 {
		t.Fatal("an untransacted version was not charged to the untracked count")
	}
}

// TestTxState_RefusesReuseWhileARecordIsStranded is the pool's safety rule.
//
// A late writer can allocate a record into a state whose owner has already
// finished. Recycling that state would publish the stranded version with the next
// transaction's timestamp, so the state must refuse to be reused.
func TestTxState_RefusesReuseWhileARecordIsStranded(t *testing.T) {
	t.Parallel()
	var ws WriteStamp
	var clk Clock
	ws.SetClock(&clk)

	var st TxState
	armAndPublish(&ws, &st, clk.NextTxID())
	// The owner finishes without ever versioning anything, so Retract leaves no
	// record behind and the state is recyclable.
	if _, n := ws.End(); n != 0 {
		t.Fatalf("an empty transaction reported %d versions, want 0", n)
	}
	if !st.Reusable() {
		t.Fatal("a cleanly retracted state is not reusable, so the pool can never hit")
	}

	// Now strand a record in it: arm, let a version allocate, and abandon the
	// state without retracting — which is what an unsynchronised public-API
	// mutator does when it finds a window that is closing under it.
	if !st.Arm(clk.NextTxID()) {
		t.Fatal("Arm refused a reusable state")
	}
	if st.Ensure() == nil {
		t.Fatal("Ensure produced no record on an armed state")
	}
	if st.Reusable() {
		t.Fatal("a state holding a stranded record reports itself reusable: the next " +
			"transaction would publish someone else's version")
	}
	if st.Arm(clk.NextTxID()) {
		t.Fatal("Arm accepted a state holding a stranded record")
	}
}

// TestWriteStamp_ConcurrentTransactionsUnderRace drives the per-transaction state
// from many goroutines at once, which is rmp #2301's acceptance instrument.
//
// Every transaction must recover exactly the record it created and exactly its
// own version count; the sum of the counts must equal the versions created, so
// nothing is lost and nothing is double-charged.
func TestWriteStamp_ConcurrentTransactionsUnderRace(t *testing.T) {
	t.Parallel()
	var ws WriteStamp
	var clk Clock
	ws.SetClock(&clk)

	const writers = 32
	const perWriter = 25

	var wg sync.WaitGroup
	wg.Add(writers)
	counts := make([]int64, writers)
	bad := make([]string, writers)
	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				var st TxState
				id := clk.NextTxID()
				if !st.Arm(id) {
					bad[w] = "Arm refused a fresh state"
					return
				}
				ws.Publish(&st)
				want := i%4 + 1
				var rec *CommitInfo
				for v := 0; v < want; v++ {
					got := st.Ensure()
					if got == nil {
						bad[w] = "Ensure returned nil inside an open window"
						return
					}
					if rec != nil && got != rec {
						bad[w] = "one transaction got two records"
						return
					}
					rec = got
				}
				// Retract through the owner, not the slot: another writer's Begin
				// may have replaced the slot, and that must not cost this
				// transaction its record.
				info, n := st.Retract()
				if info != rec {
					bad[w] = "a transaction lost its own record"
					return
				}
				if n != int64(want) {
					bad[w] = "a transaction lost its own version count"
					return
				}
				info.Commit(clk.NextCommitTS())
				counts[w] += n
			}
		}(w)
	}
	wg.Wait()

	for w, msg := range bad {
		if msg != "" {
			t.Fatalf("writer %d: %s", w, msg)
		}
	}
	var total int64
	for _, n := range counts {
		total += n
	}
	var want int64
	for i := 0; i < perWriter; i++ {
		want += int64(i%4 + 1)
	}
	want *= writers
	if total != want {
		t.Fatalf("%d versions accounted for, want %d", total, want)
	}
}
