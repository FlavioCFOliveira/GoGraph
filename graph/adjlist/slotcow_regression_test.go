package adjlist

import (
	"runtime"
	"sync"
	"testing"
)

// slotcow_regression_test.go — rmp #2882.
//
// A write outside a commit window used to publish its new shard version by
// cloning the WHOLE shard slot array, so the bytes one edge insert allocated
// grew linearly with the graph: 853 B per insert+remove at 5 000 nodes against
// 19 280 B at 150 000, measured by BenchmarkSlotCOW_Unbracketed at 127c012b.
// A write to an array no Snapshot has pinned now stores the slot in place, and
// only a write to a pinned array clones it, so the per-write cost no longer
// depends on the graph size.

// unbracketedBytesPerOp returns the heap bytes one unbracketed AddEdge plus
// RemoveEdge allocates on a graph of n nodes that each carry one edge.
func unbracketedBytesPerOp(t *testing.T, n int) float64 {
	t.Helper()
	a := New[int, float64](Config{})
	a.BeginCommit()
	for i := 0; i < n; i++ {
		if err := a.AddEdge(i, (i+1)%n, 1); err != nil {
			a.EndCommit()
			t.Fatalf("AddEdge: %v", err)
		}
	}
	a.EndCommit()
	const sentinel = -1
	if err := a.AddNode(sentinel); err != nil {
		t.Fatal(err)
	}
	const ops = 2000
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < ops; i++ {
		src := (i * 7919) % n
		if err := a.AddEdge(src, sentinel, 2); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		must(t).E(a.RemoveEdge(src, sentinel))
	}
	runtime.ReadMemStats(&after)
	// Assert the writes happened, so a pass cannot come from a loop that
	// published nothing.
	if got := a.Size(); got != uint64(n) {
		t.Fatalf("Size = %d after balanced insert/remove, want %d", got, n)
	}
	return float64(after.TotalAlloc-before.TotalAlloc) / ops
}

// TestSlotCOW_UnbracketedWriteCostIsIndependentOfGraphSize fails on the
// whole-array clone. The bound is structural, not proportional: between 5 000
// and 150 000 nodes a shard's trie gains at most one level, and each op makes
// two writes, so the per-op difference may not exceed two 256-byte nodes per
// write plus allocator rounding — 1 KiB. The whole-array clone differs by
// 18.4 KiB over the same range.
//
// Not parallel: TotalAlloc is process-global, and a non-parallel top-level test
// runs while no other test of the package does.
func TestSlotCOW_UnbracketedWriteCostIsIndependentOfGraphSize(t *testing.T) {
	small := unbracketedBytesPerOp(t, 5_000)
	large := unbracketedBytesPerOp(t, 150_000)
	t.Logf("bytes per insert+remove: %.0f at 5 000 nodes, %.0f at 150 000 nodes", small, large)
	if small <= 0 {
		t.Fatalf("measured %.0f bytes per op at 5 000 nodes; the probe observed no allocation", small)
	}
	const bound = 1024
	if large-small > bound {
		t.Fatalf("an unbracketed write on a 150 000-node graph allocates %.0f B/op against %.0f "+
			"at 5 000 nodes (difference %.0f > %d): the write's copy-on-write cost grows with "+
			"the graph, which is the whole-shard slot clone rmp #2882 removed", large, small, large-small, bound)
	}
}

// TestSlotCOW_PinnedVersionNeverChangesUnderConcurrentUnbracketedWrites pins
// the one reader the old whole-array clone existed for. Unbracketed writes now
// store in place into an unpinned array (rmp #2882); a PinSnapshot racing them
// must still capture a version that never changes afterwards. The pinner reads
// the pinned entry of a hot node twice with live writes in between and
// requires the two reads to be the same entry, while asserting that the live
// entry DID change across a pin — otherwise the test would pass without ever
// exercising the race.
func TestSlotCOW_PinnedVersionNeverChangesUnderConcurrentUnbracketedWrites(t *testing.T) {
	a := New[int, float64](Config{})
	const hub, writers, pins = 0, 4, 2000
	for i := 0; i < 64; i++ {
		if err := a.AddEdge(hub, i+1, 1); err != nil {
			t.Fatal(err)
		}
	}
	id, _ := a.Mapper().Lookup(hub)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			dst := 1000 + w
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := a.AddEdge(hub, dst, 2); err != nil {
					t.Error(err)
					return
				}
				must(t).E(a.RemoveEdge(hub, dst))
			}
		}(w)
	}

	changed := 0
	for i := 0; i < pins; i++ {
		snap := a.PinSnapshot()
		before := snap.loadEntryPinned(id)
		liveBefore := loadEntry[float64](&a.shards[id&shardMask], uint64(id)>>shardBits)
		for k := 0; k < 8; k++ {
			runtime.Gosched()
		}
		after := snap.loadEntryPinned(id)
		liveAfter := loadEntry[float64](&a.shards[id&shardMask], uint64(id)>>shardBits)
		if before != after {
			close(stop)
			wg.Wait()
			t.Fatalf("pin %d: the pinned entry changed under concurrent unbracketed writes", i)
		}
		if liveBefore != liveAfter {
			changed++
		}
	}
	close(stop)
	wg.Wait()
	if changed == 0 {
		t.Fatal("no live write landed between the two reads of any pin: the race was never exercised")
	}
	t.Logf("%d of %d pins had a live write land between their two reads", changed, pins)
}
