package adjlist_test

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestSnapshot_PinIsStableAcrossWrites is the load-bearing test for the F3.2
// copy-on-write conversion (task #1526): a pinned [adjlist.Snapshot] must
// observe the adjacency exactly as it was at pin time, regardless of writes
// published afterwards. This holds only because every write now republishes a
// fresh immutable shardSlots (copy-on-write) rather than mutating the published
// one in place.
//
// Power-check: if storeEntry is reverted to mutate the published slot array in
// place, this test trips — the pinned snapshot would observe edges added after
// the pin. It is the direct proof that the COW conversion is the mechanism the
// stage delivers (the lpg visMu barrier alone would not make a pin stable for a
// reader that outlives a writer's window — task #1671's future use case).
func TestSnapshot_PinIsStableAcrossWrites(t *testing.T) {
	t.Parallel()

	a := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})
	// Seed three edges out of node 0 across two shards' worth of dst ids.
	for _, dst := range []int{1, 2, 3} {
		if err := a.AddEdge(0, dst, int64(dst)); err != nil {
			t.Fatalf("seed AddEdge 0->%d: %v", dst, err)
		}
	}

	srcID, ok := a.Mapper().Lookup(0)
	if !ok {
		t.Fatal("node 0 not interned")
	}

	// Pin the snapshot, capturing {1,2,3}.
	snap := a.PinSnapshot()
	pinned, _ := snap.LoadEntry(srcID)
	if got := len(pinned); got != 3 {
		t.Fatalf("pinned neighbours at pin time = %d, want 3", got)
	}

	// Publish more writes AFTER the pin: add edges, remove one, relabel.
	for _, dst := range []int{4, 5, 6} {
		if err := a.AddEdge(0, dst, int64(dst)); err != nil {
			t.Fatalf("post-pin AddEdge 0->%d: %v", dst, err)
		}
	}
	a.RemoveEdge(0, 2)

	// The pinned snapshot must still see exactly {1,2,3} in order — none of the
	// post-pin additions, and not the post-pin removal of 2.
	got, _ := snap.LoadEntry(srcID)
	want := make([]graph.NodeID, 0, 3)
	for _, dst := range []int{1, 2, 3} {
		id, _ := a.Mapper().Lookup(dst)
		want = append(want, id)
	}
	if len(got) != len(want) {
		t.Fatalf("pinned snapshot after concurrent writes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pinned snapshot neighbour[%d] = %d, want %d (full: %v want %v)", i, got[i], want[i], got, want)
		}
	}

	// The LIVE adjacency, by contrast, reflects every post-pin write:
	// {1,3,4,5,6} (2 removed). This confirms the writes really happened and the
	// pin is genuinely a frozen older version, not a no-op.
	liveCount := 0
	for range a.Neighbours(0) {
		liveCount++
	}
	if liveCount != 5 {
		t.Fatalf("live neighbours after post-pin writes = %d, want 5", liveCount)
	}
}

// TestSnapshot_ReadsMatchLiveAtPinTime verifies the pinned read accessors
// (LoadEntry / LoadEntryH / LoadEntryLabels / HasEdge) return exactly what the
// live AdjList accessors return at the instant of the pin, across weights,
// handles and labels.
func TestSnapshot_ReadsMatchLiveAtPinTime(t *testing.T) {
	t.Parallel()

	a := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})
	// Edge with a handle and a label, plus a plain weighted edge.
	if err := a.AddEdgeLabeledH(0, 1, 10, 7, 99); err != nil {
		t.Fatalf("AddEdgeLabeledH: %v", err)
	}
	if err := a.AddEdge(0, 2, 20); err != nil {
		t.Fatalf("AddEdge 0->2: %v", err)
	}

	srcID, _ := a.Mapper().Lookup(0)
	snap := a.PinSnapshot()

	liveNb, liveW, liveH := a.LoadEntryH(srcID)
	snapNb, snapW, snapH := snap.LoadEntryH(srcID)
	if len(snapNb) != len(liveNb) {
		t.Fatalf("snapshot neighbours len = %d, live = %d", len(snapNb), len(liveNb))
	}
	for i := range liveNb {
		if snapNb[i] != liveNb[i] {
			t.Errorf("neighbour[%d] snap=%d live=%d", i, snapNb[i], liveNb[i])
		}
		if snapW[i] != liveW[i] {
			t.Errorf("weight[%d] snap=%d live=%d", i, snapW[i], liveW[i])
		}
		if snapH[i] != liveH[i] {
			t.Errorf("handle[%d] snap=%d live=%d", i, snapH[i], liveH[i])
		}
	}

	liveLabs := a.LoadEntryLabels(srcID)
	snapLabs := snap.LoadEntryLabels(srcID)
	if len(snapLabs) != len(liveLabs) {
		t.Fatalf("snapshot labels len = %d, live = %d", len(snapLabs), len(liveLabs))
	}
	for i := range liveLabs {
		if snapLabs[i] != liveLabs[i] {
			t.Errorf("label[%d] snap=%d live=%d", i, snapLabs[i], liveLabs[i])
		}
	}

	if !snap.HasEdge(0, 1) || !snap.HasEdge(0, 2) {
		t.Error("snapshot HasEdge missing a seeded edge")
	}
	if snap.HasEdge(0, 3) {
		t.Error("snapshot HasEdge reported a non-existent edge")
	}
}

// TestSnapshot_EmptyGraphNilSafe confirms PinSnapshot tolerates a graph with
// no writes (lazy, nil per-shard versions) and reads back as empty without
// panicking — the seeding hazard the storage auditor flagged, handled by a
// nil-tolerant pin rather than eager allocation.
func TestSnapshot_EmptyGraphNilSafe(t *testing.T) {
	t.Parallel()

	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	snap := a.PinSnapshot()

	// Any NodeID reads back empty; HasEdge on never-interned nodes is false.
	nb, w := snap.LoadEntry(graph.NodeID(0))
	if nb != nil || w != nil {
		t.Errorf("empty-graph pinned LoadEntry = (%v,%v), want (nil,nil)", nb, w)
	}
	if snap.HasEdge(0, 1) {
		t.Error("empty-graph pinned HasEdge returned true")
	}

	// After a write, a NEW pin observes it; the OLD empty pin does not.
	if err := a.AddEdge(0, 1, 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if snap.HasEdge(0, 1) {
		t.Error("old empty pin observed a post-pin write")
	}
	snap2 := a.PinSnapshot()
	if !snap2.HasEdge(0, 1) {
		t.Error("fresh pin did not observe the write")
	}
}

// TestSnapshot_ConcurrentPinUnderWrites stress-tests pin stability under a
// concurrent writer: each pinned snapshot must present a consistent prefix of
// the writer's sequential 0→1,0→2,… appends and never change after it is
// pinned. Run under -race.
func TestSnapshot_ConcurrentPinUnderWrites(t *testing.T) {
	t.Parallel()

	const N = 5_000
	numReaders := max(2, runtime.GOMAXPROCS(0)-1)

	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	srcID, _ := a.Mapper().Lookup(0)
	_ = a.AddEdge(0, 1, 1) // ensure node 0 interned with id 0's shard
	srcID, _ = a.Mapper().Lookup(0)

	start := make(chan struct{})
	writerDone := make(chan struct{})
	stop := make(chan struct{})
	var violations atomic.Int64
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(writerDone)
		<-start
		for k := 2; k <= N; k++ {
			if err := a.AddEdge(0, k, int64(k)); err != nil {
				violations.Add(1)
				return
			}
		}
	}()

	wg.Add(numReaders)
	for range numReaders {
		go func() {
			defer wg.Done()
			<-start
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap := a.PinSnapshot()
				first, _ := snap.LoadEntry(srcID)
				// A pinned read repeated must be identical (stable) and a valid
				// prefix 1,2,3,...
				for i, n := range first {
					want, _ := a.Mapper().Lookup(i + 1)
					if n != want {
						violations.Add(1)
						break
					}
				}
				second, _ := snap.LoadEntry(srcID)
				if len(first) != len(second) {
					violations.Add(1)
				}
				runtime.Gosched()
			}
		}()
	}

	close(start)
	<-writerDone
	close(stop)
	wg.Wait()

	if v := violations.Load(); v != 0 {
		t.Errorf("pin-stability/prefix violations: %d", v)
	}
	_ = srcID
}

// TestSnapshot_WindowedInPlace_ConcurrentLockFreeReader is the direct
// regression guard for the F3.2 commit-window in-place write (task #1526): a
// WINDOWED writer doing many same-shard in-place atomic.StorePointer updates
// (BeginCommit → repeated AddEdge to one hub → EndCommit) concurrently with a
// genuinely lock-free reader (LoadEntryH, the same accessor the non-blocking
// checkpointer's phase-2 handle walk uses). It exercises the exact branch the
// initial plain-store race lived on — the in-place path taken on the second and
// later writes to a shard already cloned this window — which the unwindowed
// TestSnapshot_ConcurrentPinUnderWrites does NOT reach (that path
// clones-and-publishes every op). Run under -race: the lock-free LoadEntryH
// (atomic.LoadPointer) paired with the writer's in-place atomic.StorePointer
// must never tear, and every observed entry must be a valid prefix of the hub's
// 1,2,3,… append sequence.
func TestSnapshot_WindowedInPlace_ConcurrentLockFreeReader(t *testing.T) {
	t.Parallel()

	const N = 5_000
	numReaders := max(2, runtime.GOMAXPROCS(0)-1)

	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	// Intern the hub so its NodeID/shard is fixed before the readers start.
	if err := a.AddEdge(0, 1, 1); err != nil {
		t.Fatalf("seed: %v", err)
	}
	hubID, _ := a.Mapper().Lookup(0)

	start := make(chan struct{})
	writerDone := make(chan struct{})
	stop := make(chan struct{})
	var violations atomic.Int64
	var reads atomic.Int64
	var wg sync.WaitGroup

	// Writer: one big commit window, all writes to the SAME hub shard, so every
	// op after the first takes the in-place atomic.StorePointer path.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(writerDone)
		<-start
		a.BeginCommit()
		for k := 2; k <= N; k++ {
			if err := a.AddEdge(0, k, int64(k)); err != nil {
				violations.Add(1)
				break
			}
		}
		a.EndCommit()
	}()

	// Lock-free readers: hammer LoadEntryH on the hub mid-window. Each observed
	// neighbour list must be a prefix of 1,2,3,… (the hub's append order); a
	// torn read would surface as an out-of-order or impossible neighbour id.
	wg.Add(numReaders)
	for range numReaders {
		go func() {
			defer wg.Done()
			<-start
			for {
				select {
				case <-stop:
					return
				default:
				}
				nb, _, _ := a.LoadEntryH(hubID)
				reads.Add(1)
				for i, n := range nb {
					want, ok := a.Mapper().Lookup(i + 1)
					if !ok || n != want {
						violations.Add(1)
						break
					}
				}
				runtime.Gosched()
			}
		}()
	}

	close(start)
	<-writerDone
	close(stop)
	wg.Wait()

	if v := violations.Load(); v != 0 {
		t.Errorf("windowed in-place write vs lock-free reader: %d violations", v)
	}
	if reads.Load() == 0 {
		t.Fatal("readers never read; test did not exercise the in-place path")
	}
}
