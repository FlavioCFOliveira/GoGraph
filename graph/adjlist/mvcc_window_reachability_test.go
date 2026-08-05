package adjlist

// mvcc_window_reachability_test.go — rmp #2327: an MVCC snapshot reader DOES reach a
// shard whose builder is being mutated in place inside an open commit window, and that
// is sound for reasons that have nothing to do with visMu.
//
// Layer: short.
//
// # The question this answers
//
// Two comments in adjlist.go justified the in-place builder mutation by claiming that
// "concurrent readers cannot run during a window (it is held under visMu.Lock while
// reads are under visMu.RLock)". Sprint 334 made that premise false: the MVCC read path
// resolves through a start timestamp and takes no barrier at all. The open question was
// whether a snapshot reader can therefore observe another transaction's in-flight
// adjacency — an isolation violation.
//
// It can reach the slot. It cannot observe the in-flight state. The soundness rests on
// four properties, none of them visMu, and each is asserted here:
//
//  1. the in-window write is an atomic.StorePointer (adjlist.go:2515) paired with
//     loadEntry's atomic.LoadPointer (adjlist.go:2343), so the pointer is never torn;
//  2. an adjEntry is IMMUTABLE once published — a write replaces the pointer, never the
//     entry — so a complete old-or-new entry is all a reader can see;
//  3. the slot ARRAY is never resized in place: growth allocates a fresh shardSlots and
//     republishes slotsRef (adjlist.go:2488-2496), so a reader holding an older array
//     has a stable slice header;
//  4. ISOLATION comes from the version chain, not from exclusion: linkVersion runs
//     before any branch publishes (adjlist.go:2477), so every in-window write records
//     the entry it supersedes and a reader whose start timestamp precedes the commit
//     steps back to it.
//
// # Why the first test carries its own positive control
//
// "The reader never saw a bad value" is worthless unless a bad value was there to be
// seen. The task that filed this was explicit that the absence of a reproduction is not
// evidence of safety, and this sprint has twice had a window that looked unreachable
// turn out to be real.
//
// So the deterministic test reads the SAME slot twice at the same instant, once through
// the present-time path (LoadEntry, no version walk) and once through the snapshot path
// (EntryNeighboursAsOf). The present-time read MUST observe the in-flight state. That is
// the control: it proves the window is open, the mutation has landed, and an unversioned
// reader of this very slot would see it — so the snapshot read's correct answer is a
// result rather than an accident of timing.

import (
	"runtime"
	"sync"
	"testing"
)

// TestLoadEntry_SnapshotReaderReachesAnOpenWindowAndStepsBackOverIt is the deterministic
// half, and the one that settles the reachability question the task asked.
func TestLoadEntry_SnapshotReaderReachesAnOpenWindowAndStepsBackOverIt(t *testing.T) {
	a, clk := versionedList(t)
	for _, n := range []string{"a", "b", "c", "d"} {
		if err := a.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	id := idOf(t, a, "a")
	s := &a.shards[id&shardMask]

	// A committed past, so "step back" has somewhere to step to.
	_, tsBefore := txWrite(a, clk, func() {
		if err := a.AddEdge("a", "b", 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	})

	// Open a window and write the same shard twice. The second write is the in-place
	// builder mutation; the first is the clone-and-publish that creates the builder.
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

	// The shortcut must actually have been taken, or the window under test is an
	// ordinary copy-on-write and nothing here means anything.
	if arrayAfterFirst != arrayAfterSecond {
		t.Fatal("the second same-shard write inside the window published a NEW slot array, " +
			"so the in-place builder mutation this test is about did not happen")
	}

	// ── THE POSITIVE CONTROL ─────────────────────────────────────────────────────
	// The present-time path reads the same slot with no version walk. It MUST see the
	// in-flight state: that is what proves the window is reachable, that the mutation
	// has landed in a published array, and that this instrument can detect an in-flight
	// read at all. If this assertion ever fails, the test below has stopped being
	// informative and must not be trusted as evidence of isolation.
	live, _ := a.LoadEntry(id)
	if len(live) != 3 {
		t.Fatalf("the PRESENT-TIME read of the slot sees %d neighbours, want 3 (the "+
			"in-flight window state). Either the window is not reachable through "+
			"loadEntry after all, or the mutation did not land — either way the "+
			"snapshot assertion below proves nothing and this test is void", len(live))
	}

	// ── THE ASSERTION ────────────────────────────────────────────────────────────
	// The same slot, the same instant, resolved through the version chain by a reader
	// whose start timestamp precedes the open transaction. It must see the COMMITTED
	// adjacency and none of the in-flight writes the control just observed.
	if got := a.EntryNeighboursAsOf(id, tsBefore, 0); len(got) != 1 {
		t.Fatalf("a SNAPSHOT reader from before the open transaction sees %d neighbours, "+
			"want 1. The present-time control above proves the in-flight state is "+
			"reachable at this slot, so this is a reader observing another "+
			"transaction's uncommitted adjacency: an isolation violation", len(got))
	}

	// And after the commit it sees all of it, atomically.
	a.EndCommit()
	info, _ := ws.End()
	if info == nil {
		t.Fatal("the transaction versioned nothing, so no version chain was exercised")
	}
	tsAfter := clk.NextCommitTS()
	info.Commit(tsAfter)
	if got := a.EntryNeighboursAsOf(id, tsAfter, 0); len(got) != 3 {
		t.Fatalf("a snapshot reader from after the commit sees %d neighbours, want 3", len(got))
	}
	if got := a.EntryNeighboursAsOf(id, tsBefore, 0); len(got) != 1 {
		t.Fatalf("after the commit, the reader from BEFORE it now sees %d neighbours, "+
			"want 1: committing must not retroactively change what an older snapshot reads", got)
	}
}

// TestLoadEntry_ConcurrentSnapshotReadersNeverObserveAnOpenWindow is the concurrent
// half. It exists for two reasons the deterministic test cannot serve: it gives `-race`
// the store/load pair to judge across goroutines, and it samples the window at instants
// no single-threaded test can choose.
//
// THE ORACLE IS ARITHMETIC, not a fixed expectation. Every transaction adds exactly
// windowFanout edges to the same node inside one window, so every COMMITTED neighbour
// count is a multiple of windowFanout and every INTERMEDIATE count is not. A reader that
// observes a non-multiple has observed an open window.
//
// Run under -race.
func TestLoadEntry_ConcurrentSnapshotReadersNeverObserveAnOpenWindow(t *testing.T) {
	// txCount is large enough, and the window deliberately yields between writes, so
	// that the write phase outlasts goroutine start-up. The first draft used 60
	// transactions with no yield: the whole write phase finished in under a
	// millisecond, the readers observed exactly one count, and the guard at the bottom
	// of this test refused to pass on that — correctly, since a reader that never
	// overlaps the writer cannot detect anything.
	const (
		windowFanout = 8
		txCount      = 300
		readers      = 8
	)

	a, clk := versionedList(t)
	if err := a.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	// Distinct destinations, so every AddEdge is a fresh neighbour and the count is a
	// clean function of how many writes have landed.
	dst := make([]string, 0, windowFanout*txCount)
	for i := 0; i < windowFanout*txCount; i++ {
		n := "d" + itoaTest(i)
		if err := a.AddNode(n); err != nil {
			t.Fatalf("AddNode %s: %v", n, err)
		}
		dst = append(dst, n)
	}
	id := idOf(t, a, "a")

	var (
		wg       sync.WaitGroup
		ready    sync.WaitGroup
		stop     = make(chan struct{})
		mu       sync.Mutex
		bad      []int
		observed = map[int]struct{}{}
	)

	ready.Add(readers)
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			first := true
			for {
				select {
				case <-stop:
					return
				default:
				}
				got := len(a.EntryNeighboursAsOf(id, clk.ReadTS(), 0))
				mu.Lock()
				observed[got] = struct{}{}
				if got%windowFanout != 0 {
					bad = append(bad, got)
				}
				mu.Unlock()
				if first {
					// Signal only after a completed read, so "ready" means the
					// reader is actually sampling and not merely scheduled.
					first = false
					ready.Done()
				}
			}
		}()
	}
	// Do not start writing until every reader is sampling. Without this the writer
	// can finish before the runtime has started the reader goroutines at all.
	ready.Wait()

	for tx := 0; tx < txCount; tx++ {
		ws := a.WriteStampForTest()
		beginTx(ws)
		a.BeginCommit()
		for i := 0; i < windowFanout; i++ {
			if err := a.AddEdge("a", dst[tx*windowFanout+i], 1); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
			// Hold the window open across a scheduling point, so readers get to
			// sample the shard mid-window rather than only between transactions.
			// This is what makes the test exercise the in-place mutation under a
			// concurrent reader instead of merely running one nearby.
			runtime.Gosched()
		}
		a.EndCommit()
		info, _ := ws.End()
		if info != nil {
			ts := clk.NextCommitTS()
			info.Commit(ts)
			// PUBLISH, or the readers see nothing. CommitInfo.Commit only stamps the
			// record; the clock's visible frontier advances via PublishCommitTS
			// (mvcc.go:158), and a reader starting at clk.ReadTS() reads at that
			// frontier. Omitting this pins every reader at start timestamp 0, where
			// the node has no edges and no assertion about what a reader sees can
			// ever fire — which is exactly how the pre-existing companion test in
			// mvcc_window_test.go came to be vacuous.
			clk.PublishCommitTS(ts)
		}
	}
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(bad) > 0 {
		t.Fatalf("snapshot readers observed %d neighbour counts that no committed version "+
			"ever had (e.g. %v; every committed count is a multiple of %d): an in-window "+
			"state leaked past the version chain, which is a reader seeing another "+
			"transaction's uncommitted adjacency",
			len(bad), bad[:minIntTest(4, len(bad))], windowFanout)
	}
	// The readers must actually have raced the writer. If they only ever saw one count
	// they were scheduled outside the whole write phase and the absence of a violation
	// says nothing.
	if len(observed) < 2 {
		t.Fatalf("the readers observed only %d distinct count(s), so they never overlapped "+
			"the writer and this test had no opportunity to detect a violation", len(observed))
	}
	if got := len(a.EntryNeighboursAsOf(id, clk.ReadTS(), 0)); got != windowFanout*txCount {
		t.Fatalf("after every transaction committed the node has %d neighbours, want %d",
			got, windowFanout*txCount)
	}
}

// itoaTest keeps the fixture names allocation-simple without pulling strconv into this
// file for one call site.
func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
