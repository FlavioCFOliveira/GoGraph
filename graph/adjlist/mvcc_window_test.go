package adjlist

// mvcc_window_test.go — the in-window in-place builder mutation, under
// versioning (rmp #2288).
//
// # Why this test exists
//
// storeEntry's comment used to say the in-place builder mutation was a
// barrier-borrowed shortcut, and that a lock-free read path would turn it into
// a torn read, so MVCC P4 would have to remove it and pay O(ops) per commit
// instead of O(distinct shards touched). The analysis that replaced that
// comment says the opposite: the mutation swaps a slot POINTER with an atomic
// store, never touches an entry, and with versioning armed the new entry
// carries a record of the one it replaced, so a reader from before the write
// steps back to exactly what it should see.
//
// A cost that large must not rest on an argument alone, so both halves of the
// argument are asserted here:
//
//   - the SHORTCUT IS TAKEN — the second write to a shard within a window
//     mutates the same slot array rather than publishing a new one, checked by
//     pointer identity, so the test cannot silently stop exercising the thing
//     it is about;
//   - the READ IS CORRECT across it, in both directions: a reader from before
//     the transaction sees none of it, and one from after sees all of it.
//
// The concurrent companion runs the two against each other so `-race` has
// something to say about the atomic store this rests on.

import (
	"sync"
	"testing"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// TestStoreEntry_InPlaceWindowMutationIsSoundUnderVersioning pins that a
// transaction which writes the same node twice inside one commit window leaves
// a reader from before it seeing the pre-transaction adjacency.
func TestStoreEntry_InPlaceWindowMutationIsSoundUnderVersioning(t *testing.T) {
	a, clk := versionedList(t)
	for _, n := range []string{"a", "b", "c", "d"} {
		if err := a.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	id := idOf(t, a, "a")
	s := &a.shards[id&shardMask]
	intra := uint64(id) >> shardBits

	// One committed edge, so there is a past to preserve.
	_, tsBefore := txWrite(a, clk, func() {
		if err := a.AddEdge("a", "b", 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	})

	// A transaction that writes node a TWICE inside one window. The second
	// write is the in-place builder mutation this test is about.
	ws := a.WriteStampForTest()
	beginTx(ws)
	a.BeginCommit()
	if err := a.AddEdge("a", "c", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	arrayAfterFirst := s.slotsRef.Load()
	if err := a.AddEdge("a", "d", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	arrayAfterSecond := s.slotsRef.Load()
	a.EndCommit()
	info, _ := ws.End()
	if info == nil {
		t.Fatal("the transaction versioned nothing, so this test exercises no version chain")
	}
	tsAfter := clk.NextCommitTS()
	info.Commit(tsAfter)

	// Half one: the shortcut was actually taken. Without this the test could
	// pass forever while measuring an ordinary copy-on-write.
	if arrayAfterFirst != arrayAfterSecond {
		t.Fatal("the second same-shard write inside the window published a NEW slot array, so the " +
			"in-place builder mutation this test exists to validate did not happen")
	}
	if loadSlot(arrayAfterSecond, intra) == nil {
		t.Fatal("no entry published at the node's slot")
	}

	// Half two: the read is correct on both sides of the transaction.
	if got := a.EntryNeighboursAsOf(id, tsBefore, 0); len(got) != 1 {
		t.Fatalf("a reader from BEFORE the transaction sees %d neighbours, want 1: the in-place "+
			"mutation was observed as a torn read instead of being stepped back over", len(got))
	}
	if got := a.EntryNeighboursAsOf(id, tsAfter, 0); len(got) != 3 {
		t.Fatalf("a reader from AFTER the transaction sees %d neighbours, want 3 — all of them, "+
			"atomically", len(got))
	}
}

// TestStoreEntry_InPlaceWindowMutationIsRaceFree drives a lock-free reader
// against a writer that mutates the window builder in place, so the race
// detector has the access pair to judge and the reader has the chance to
// observe an intermediate state.
//
// The reader's assertion is not "the count never changes" — it must change, at
// the commit boundary — but that every count it observes is one a COMMITTED
// version actually had. An intermediate in-window state has a count no
// committed version ever held, so observing one fails here.
func TestStoreEntry_InPlaceWindowMutationIsRaceFree(t *testing.T) {
	a, clk := versionedList(t)
	const fanout = 6
	keys := []string{"a"}
	for i := 0; i < fanout; i++ {
		keys = append(keys, string(rune('b'+i)))
	}
	for _, n := range keys {
		if err := a.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	id := idOf(t, a, "a")

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var bad []int
	var mu sync.Mutex

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			startTS := clk.ReadTS()
			got := len(a.EntryNeighboursAsOf(id, startTS, 0))
			// The writer commits ONE transaction that adds all `fanout` edges,
			// so the only committed counts are 0 and fanout.
			if got != 0 && got != fanout {
				mu.Lock()
				bad = append(bad, got)
				mu.Unlock()
			}
		}
	}()

	// One transaction, `fanout` in-place writes to the same node in one window.
	ws := a.WriteStampForTest()
	beginTx(ws)
	a.BeginCommit()
	for i := 0; i < fanout; i++ {
		if err := a.AddEdge("a", string(rune('b'+i)), 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	a.EndCommit()
	info, _ := ws.End()
	info.Commit(clk.NextCommitTS())

	// Give the reader a window on the far side of the commit too.
	for i := 0; i < 2000; i++ {
		_ = a.EntryNeighboursAsOf(id, clk.ReadTS(), 0)
	}
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(bad) > 0 {
		t.Fatalf("a lock-free reader observed %d intermediate neighbour counts (e.g. %v); only 0 "+
			"and %d were ever committed, so an in-window state leaked past the version chain",
			len(bad), bad[:minIntTest(4, len(bad))], fanout)
	}
}

// TestAdjVersion_UnstampedWriteIsVisibleOnlyAfterwards pins the direct-mutation
// path: a write made with no transaction open takes its own commit timestamp,
// so a reader that started before it still does not see it. Without a clock on
// the stamp such a write is timestamped zero and every reader sees it, which is
// the regression this guards.
func TestAdjVersion_UnstampedWriteIsVisibleOnlyAfterwards(t *testing.T) {
	a, clk := versionedList(t)
	for _, n := range []string{"a", "b"} {
		if err := a.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	id := idOf(t, a, "a")
	before := clk.ReadTS()
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if got := len(a.EntryNeighboursAsOf(id, before, 0)); got != 0 {
		t.Fatalf("a reader from before an unstamped write sees %d neighbours, want 0: the write "+
			"was timestamped zero, which makes it visible to every reader that ever existed", got)
	}
	if got := len(a.EntryNeighboursAsOf(id, clk.ReadTS(), 0)); got != 1 {
		t.Fatalf("a reader from after an unstamped write sees %d neighbours, want 1", got)
	}
}

// TestWriteStamp_AllocatesOnlyWhenAVersionNeedsIt pins the lazy allocation that
// keeps an empty transaction bracket allocation-free — the property
// TestBarrierGuard_ApplyAtomicallyAllocatesNothing depends on one layer up.
func TestWriteStamp_AllocatesOnlyWhenAVersionNeedsIt(t *testing.T) {
	var ws mvcc.WriteStamp
	ws.SetClock(&mvcc.Clock{})

	beginTx(&ws)
	if info, n := ws.End(); info != nil || n != 0 {
		t.Fatalf("an empty transaction produced a commit record (%v) and %d versions, want none of "+
			"either: the record must be allocated by the first version that needs one", info, n)
	}

	beginTx(&ws)
	first, _ := ws.Stamp()
	second, _ := ws.Stamp()
	if first == nil || first != second {
		t.Fatalf("two versions in one transaction got records %v and %v, want one shared record",
			first, second)
	}
	info, n := ws.End()
	if info != first || n != 2 {
		t.Fatalf("End returned (%v, %d), want (%v, 2)", info, n, first)
	}
}

func minIntTest(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// loadSlot reads the raw slot pointer, so the test can assert an entry is
// present without duplicating loadEntry's bounds logic.
func loadSlot(ss *shardSlots, intraIdx uint64) unsafe.Pointer {
	if ss == nil || intraIdx >= uint64(len(ss.slots)) {
		return nil
	}
	return ss.slots[intraIdx]
}
