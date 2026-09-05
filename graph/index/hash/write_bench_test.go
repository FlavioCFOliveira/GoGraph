package hash

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// benchInlineTierMax mirrors index.NodeSet's inline/bitmap tier bound.
//
// It is declared locally rather than referring to [inlineTierMax] so that this
// file compiles UNCHANGED against the pre-#2692 baseline, which has no such
// constant. The A/B arms therefore run byte-identical benchmark code, which is
// the only way the comparison means anything.
const benchInlineTierMax = 8

// benchBatch is how many distinct keys the timed loop cycles over between
// resets.
//
// Every one of these benchmarks times a write that CHANGES its key's tier, so
// the same key cannot be written twice in a row without measuring something
// else the second time. The pool of keys is primed to the target tier once,
// then the loop runs in batches: benchBatch timed writes, then — with the timer
// stopped, which excludes both its time and its allocations from the result —
// the inverse write that puts those keys back on the target tier.
//
// The reset is deliberately ONE inverse write per timed write, not a rebuild of
// the pool. A rebuild would make the untimed setup proportional to the tier's
// preload depth and to b.N, and since b.N is chosen from the TIMED duration
// alone, the framework would escalate it until the untimed half ran for
// minutes. This shape keeps the untimed half proportional to b.N with a small
// constant, so a plain `go test -bench=.` is well behaved.
const benchBatch = 512

// primeTier returns an index holding benchBatch keys, each carrying preload
// ids, so a timed write against key k is a write against a list of exactly that
// cardinality.
func primeTier(preload int) *Index[int64] {
	idx := New[int64]()
	for k := range benchBatch {
		for p := range preload {
			idx.Insert(int64(k), graph.NodeID(uint64(p)+1))
		}
	}
	return idx
}

// BenchmarkIndex_InsertByTier measures the WRITER cost of the copy-on-write
// publication that round two of rmp #2692 introduced, per starting tier.
//
// This is the bill for the lock-free read path, and it must be read honestly.
// Before round two an inline-tier insert mutated the shard's NodeSet in place;
// now it copies the two-word header, applies the insert to the copy, and
// publishes a new *snapshot — so it allocates a 24-byte snapshot where the old
// code allocated nothing beyond whatever the NodeSet tier itself needed. The
// sub-benchmarks isolate each tier so the cost is attributed rather than
// averaged:
//
//   - Create — the key is absent; Insert allocates the entry and its first
//     snapshot and publishes them under the shard write lock.
//   - Singleton — one id present; the insert copies to the small tier.
//   - Small — benchInlineTierMax-1 ids; the insert stays on the small tier.
//   - Promote — exactly benchInlineTierMax ids; the insert crosses the bound and
//     builds a roaring bitmap. Dominated by roaring, not by the snapshot.
//   - Bitmap — already on the bitmap tier; the insert mutates the aliased
//     bitmap IN PLACE and publishes nothing, so it should be unchanged from
//     baseline.
//   - Duplicate — the id is already present; nothing changes, so nothing is
//     published and nothing is allocated. This is the steady-state re-index
//     write, and TestHotPathsAreAllocationFree pins it at zero allocations.
func BenchmarkIndex_InsertByTier(b *testing.B) {
	cases := []struct {
		name    string
		preload int
		// dupe times an insert of an id that is already present, which needs no
		// per-batch setup because it changes nothing.
		dupe bool
		// reprime rebuilds each key from empty during the reset instead of
		// removing just the id that was added.
		//
		// It is REQUIRED for Promote, and the reason is a divergence between the
		// arms rather than a preference. index.NodeSet never demotes on its own,
		// so in the pre-#2692 baseline the cheap reset (remove the id just
		// added) leaves the key on the BITMAP tier at benchInlineTierMax ids,
		// and every later timed insert is an in-place bitmap add rather than a
		// promotion. The round-two arm demotes, so it really does re-promote.
		// The cheap reset therefore times two different operations in the two
		// arms and the comparison is meaningless — as it measurably was:
		// baseline Promote read 36.20 ns at 0 allocs/op, identical to its own
		// Bitmap sub-benchmark. Rebuilding from empty makes every arm start each
		// timed insert from an inline list of exactly benchInlineTierMax ids.
		reprime bool
	}{
		{name: "Create", preload: 0},
		{name: "Singleton", preload: 1},
		{name: "Small", preload: benchInlineTierMax - 1},
		{name: "Promote", preload: benchInlineTierMax, reprime: true},
		{name: "Bitmap", preload: 32},
		{name: "Duplicate", preload: 2, dupe: true},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			if c.dupe {
				idx := New[int64]()
				for p := range c.preload {
					idx.Insert(0, graph.NodeID(uint64(p)+1))
				}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					idx.Insert(0, graph.NodeID(1))
				}
				return
			}
			newID := graph.NodeID(uint64(c.preload) + 1)
			idx := primeTier(c.preload)
			b.ResetTimer()
			b.StopTimer()
			for done := 0; done < b.N; {
				n := benchBatch
				if left := b.N - done; left < n {
					n = left
				}
				b.StartTimer()
				for k := range n {
					idx.Insert(int64(k), newID)
				}
				b.StopTimer()
				// Untimed reset: put every key touched back on the tier this
				// sub-benchmark names. Removing the id just added suffices
				// except where reprime says otherwise; for preload 0 it drops
				// the key entirely, which is what makes the next round a Create
				// again.
				for k := range n {
					idx.Delete(int64(k), newID)
					if !c.reprime {
						continue
					}
					for p := range c.preload {
						idx.Delete(int64(k), graph.NodeID(uint64(p)+1))
					}
					for p := range c.preload {
						idx.Insert(int64(k), graph.NodeID(uint64(p)+1))
					}
				}
				done += n
			}
		})
	}
}

// BenchmarkIndex_DeleteDemote measures the one write round two adds that has no
// counterpart in the baseline: the DEMOTION a [Index.Delete] pays when it takes
// a bitmap-tier posting list back down to benchInlineTierMax.
//
// index.NodeSet never demotes on its own, so before round two this Delete was
// an in-place roaring Remove and nothing else. Now it also extracts the ids,
// builds an inline set, and publishes a new snapshot — the price of putting the
// key back on the lock-free read path and back on the cheap memory tier. The
// Steady sub-benchmark is the control: a Delete that leaves the list on the
// bitmap tier still publishes nothing.
func BenchmarkIndex_DeleteDemote(b *testing.B) {
	cases := []struct {
		name    string
		preload int
	}{
		{name: "Demote", preload: benchInlineTierMax + 1},
		{name: "Steady", preload: 32},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			victim := graph.NodeID(uint64(c.preload))
			idx := primeTier(c.preload)
			b.ResetTimer()
			b.StopTimer()
			for done := 0; done < b.N; {
				n := benchBatch
				if left := b.N - done; left < n {
					n = left
				}
				b.StartTimer()
				for k := range n {
					idx.Delete(int64(k), victim)
				}
				b.StopTimer()
				// Untimed reset: put the removed id back, which returns every
				// key touched to the tier this sub-benchmark names.
				for k := range n {
					idx.Insert(int64(k), victim)
				}
				done += n
			}
		})
	}
}

// BenchmarkIndex_CardinalityInlineTier reads an INLINE-tier posting list, which
// is the tier the lock-free read path of round two actually serves.
//
// BenchmarkIndex_LookupHot cannot show that win: its hot key holds ~488 ids, so
// it is on the bitmap tier and still goes through the entry read lock by design.
// These two sub-benchmarks separate the two effects the round-one regression
// mixed together:
//
//   - Hot — the same inline key every iteration, so its cache line is warm.
//     Isolates the cost of the read lock itself.
//   - Spread — 100 000 distinct singleton keys cycled, so every read touches a
//     cold line. Isolates the cost of WRITING to that cold line, which is what
//     an RWMutex read lock does and what made round one 84.65% slower here.
func BenchmarkIndex_CardinalityInlineTier(b *testing.B) {
	b.Run("Hot", func(b *testing.B) {
		idx := New[int]()
		for v := 0; v < 2048; v++ {
			for n := range 4 {
				idx.Insert(v, graph.NodeID(uint64(n)+1))
			}
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = idx.Cardinality(42)
		}
	})

	b.Run("Spread", func(b *testing.B) {
		const keys = 100_000
		idx := New[int]()
		for v := 0; v < keys; v++ {
			idx.Insert(v, graph.NodeID(uint64(v)))
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = idx.Cardinality(i % keys)
		}
	})
}

// BenchmarkIndex_DistinctKeyFootprint measures the RETAINED heap cost of one
// distinct indexed value, PER TIER, which is the resource vector the per-entry
// design spends to buy the lock-free read path.
//
// It is reported per key rather than per operation, and it is measured — heap
// in use after two forced collections with the index still live, minus heap in
// use before it was built — rather than computed from struct sizes, because the
// size classes and the map's own growth are part of the real answer.
//
// # It is per tier because the AGGREGATE hid the problem
//
// Round two's overhead is a near-constant +58 to +65 B per key: one *entry plus
// one *snapshot where the pre-#2692 baseline held an [index.NodeSet] by value
// inside the shard map. A constant is a small fraction of a wide posting list
// and a catastrophe for a singleton, so any measurement that averages the tiers
// together reports a number that belongs to none of them, and the tier where
// the bill actually lands — the singleton, which a wide equality index is
// almost entirely made of — is exactly the one an average buries. Each tier is
// therefore built and measured on its own, at a key count that keeps the
// working set comparable:
//
//   - singleton, 200 000 keys — an email or external-id column, and the
//     cheapest possible case, so this is the FLOOR of the per-key cost.
//   - small, 20 000 keys x 4 ids — the inline backing-array tier.
//   - bitmap, 2 000 keys x 40 ids — the promoted tier, whose reads take the
//     entry read lock.
//
// Run it with a small explicit iteration count (`-benchtime=3x`): each
// iteration builds the whole index and forces four collections, so its wall
// clock is GC time and says nothing about the write path.
func BenchmarkIndex_DistinctKeyFootprint(b *testing.B) {
	cases := []struct {
		name string
		keys int
		ids  int
	}{
		{name: "singleton", keys: 200_000, ids: 1},
		{name: "small", keys: 20_000, ids: 4},
		{name: "bitmap", keys: 2_000, ids: 40},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			var retained uint64
			for i := 0; i < b.N; i++ {
				// Twice, because one collection can leave work the next one
				// finishes, and the delta below is a difference of two settled
				// readings or it is noise.
				runtime.GC()
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				idx := New[int64]()
				for v := 0; v < c.keys; v++ {
					for n := range c.ids {
						idx.Insert(int64(v), graph.NodeID(uint64(v*c.ids+n)+1))
					}
				}
				runtime.GC()
				runtime.GC()
				runtime.ReadMemStats(&after)
				retained += after.HeapAlloc - before.HeapAlloc
				runtime.KeepAlive(idx)
			}
			b.ReportMetric(float64(retained)/float64(b.N)/float64(c.keys), "B/key")
			b.ReportMetric(0, "ns/op") // the wall clock here is GC, not the write path
		})
	}
}

// BenchmarkIndex_DeleteWideBitmap probes the COMPLEXITY of a single-id delete
// against a posting list spread over many roaring containers.
//
// roaring64's GetCardinality walks every container and sums their cardinalities;
// IsEmpty reads one slice length. A delete path that consults the former is
// therefore O(containers) where one that consults the latter is O(1), and the
// difference is invisible on a benchmark whose key occupies a single container.
// The ids here are spaced 1<<16 apart, so every id lands in a container of its
// own and the container count equals the cardinality — which is what makes this
// probe able to tell the two shapes apart at all.
//
// The timed operation is one Delete; the re-Insert that restores the key is
// untimed. A slope across the two widths is the signal; a flat pair is the
// absence of one.
func BenchmarkIndex_DeleteWideBitmap(b *testing.B) {
	for _, width := range []int{1024, 65536} {
		b.Run(fmt.Sprintf("ids=%d", width), func(b *testing.B) {
			const value = int64(0)
			idx := New[int64]()
			for i := range width {
				idx.Insert(value, graph.NodeID(uint64(i)<<16))
			}
			victim := graph.NodeID(uint64(width-1) << 16)
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for i := 0; i < b.N; i++ {
				b.StartTimer()
				idx.Delete(value, victim)
				b.StopTimer()
				idx.Insert(value, victim)
			}
		})
	}
}

// spineBenchKeys is the key count the contention observatory's index-hash-rw
// workload uses, so the two instruments measure the same fixture.
const spineBenchKeys = 100_000

// BenchmarkIndex_SpineParallel is the CHEAP local gate on the lock-free shard
// spine (rmp #2699). bench/contention is the authoritative instrument for this
// surface, but it is opt-in and costs minutes; these two run in seconds and
// would catch a reintroduced shared-line write on either path.
//
// The numbers that motivated the geometry, measured on an Apple M4 against a
// reconstruction of the pre-#2699 map-behind-an-RWMutex spine in the same
// binary, benchstat over n=10:
//
//	Cardinality serial    14.93n -> 11.09n   -25.70% (p=0.000)
//	Cardinality parallel   9.522n ->  2.075n  -78.20% (p=0.000)
//	Insert      serial    28.85n -> 22.50n   -22.01% (p=0.000)
//	Insert      parallel  14.305n ->  6.070n  -57.57% (p=0.000)
//
// Both paths are allocation-free and must stay so; ReportAllocs is on for that
// reason rather than for decoration.
func BenchmarkIndex_SpineParallel(b *testing.B) {
	seedIndex := func() *Index[int64] {
		idx := New[int64]()
		for v := range spineBenchKeys {
			idx.Insert(int64(v), graph.NodeID(uint64(v))) // G115: bounded loop index
		}
		return idx
	}

	// Cardinality is the 90% side of the mixed workload: a pure read.
	b.Run("Cardinality", func(b *testing.B) {
		idx := seedIndex()
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				_ = idx.Cardinality(int64(i % spineBenchKeys))
				i++
			}
		})
	})

	// Insert against an EXISTING key is the 10% side: it takes the entry lock
	// but never the shard writer lock, which is the case the spine must not
	// serialise.
	b.Run("InsertExisting", func(b *testing.B) {
		idx := seedIndex()
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				idx.Insert(int64(i%spineBenchKeys), graph.NodeID(spineBenchKeys+1))
				i++
			}
		})
	})
}

// BenchmarkIndex_DeleteChurn measures the STEADY-STATE cost of a Delete that
// empties its value, which is the path that reaps and therefore the path
// [reclaimTrigger] taxes.
//
// Each iteration inserts one fresh key and removes one old one, so the
// population stays flat and tombstones accrue at the rate a real churning index
// produces them. A benchmark that only inserted would never reap, and one that
// only deleted would drain the fixture and stop measuring the steady state.
func BenchmarkIndex_DeleteChurn(b *testing.B) {
	const window = 50_000
	idx := New[int64]()
	for v := range window {
		idx.Insert(int64(v), graph.NodeID(uint64(v))) // G115: bounded loop index
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		k := int64(window + i)
		idx.Insert(k, graph.NodeID(1))
		idx.Delete(int64(i), graph.NodeID(uint64(i%window))) // G115: bounded loop index
	}
}

// BenchmarkIndex_DeleteChurnSparse is the same churn on a SPARSE index — fewer
// keys than shards — which is where the reclamation floor earns its place.
//
// With 256 shards a small index puts one or two keys in each, so a ratio-only
// reclaim trigger fires on almost every reap and rebuilds a table per Delete.
// See [reclaimTrigger].
func BenchmarkIndex_DeleteChurnSparse(b *testing.B) {
	const window = 400
	idx := New[int64]()
	for v := range window {
		idx.Insert(int64(v), graph.NodeID(uint64(v))) // G115: bounded loop index
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx.Insert(int64(window+i), graph.NodeID(1))
		idx.Delete(int64(i), graph.NodeID(uint64(i%window))) // G115: bounded loop index
	}
}
