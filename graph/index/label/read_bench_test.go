package label

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// The benchmarks here exist to hold a stated acceptance criterion of rmp #2685:
// the per-label-entry geometry must not make the READ paths slower, and
// [Index.IntersectCardinality] must stay at ZERO allocations, because the
// planner calls it precisely to avoid the materialisation [Index.Intersect]
// pays for.
//
// They use only the exported surface, so the same file measures any geometry
// the package might hold. Sizes span the inline small-set tier and the roaring
// bitmap tier, since the two have different read costs.

// benchSizes are the label cardinalities every read benchmark walks.
var benchSizes = []int{100, 2000, 100_000}

// buildLabel fills lbl with n ids starting at base, one at a time, so the set
// reaches its natural tier rather than the run-container AddRange shortcut.
func buildLabel(idx *Index, lbl uint32, base, n int) {
	for k := range n {
		idx.Add(lbl, graph.NodeID(base+k))
	}
}

func BenchmarkReadCount(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			idx := NewIndex()
			buildLabel(idx, 1, 0, n)
			b.ReportAllocs()
			b.ResetTimer()
			var sink uint64
			for range b.N {
				sink += idx.Count(1)
			}
			runtimeSink(sink)
		})
	}
}

func BenchmarkReadHas(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			idx := NewIndex()
			buildLabel(idx, 1, 0, n)
			b.ReportAllocs()
			b.ResetTimer()
			var sink int
			for k := range b.N {
				if idx.Has(1, graph.NodeID(k%n)) {
					sink++
				}
			}
			runtimeSink(uint64(sink))
		})
	}
}

func BenchmarkReadScan(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			idx := NewIndex()
			buildLabel(idx, 1, 0, n)
			b.ReportAllocs()
			b.ResetTimer()
			var sink int
			for range b.N {
				sink += len(idx.Scan(1))
			}
			runtimeSink(uint64(sink))
		})
	}
}

func BenchmarkReadIntersectOne(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			idx := NewIndex()
			buildLabel(idx, 1, 0, n)
			b.ReportAllocs()
			b.ResetTimer()
			var sink uint64
			for range b.N {
				sink += idx.Intersect(1).GetCardinality()
			}
			runtimeSink(sink)
		})
	}
}

func BenchmarkReadIntersectTwo(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			idx := NewIndex()
			buildLabel(idx, 1, 0, n)
			buildLabel(idx, 2, n/2, n)
			b.ReportAllocs()
			b.ResetTimer()
			var sink uint64
			for range b.N {
				sink += idx.Intersect(1, 2).GetCardinality()
			}
			runtimeSink(sink)
		})
	}
}

// BenchmarkReadIntersectCardinality is the one that must report 0 B/op. The
// planner's whole reason for calling it instead of Intersect is that sizing an
// intersection must not cost the materialisation of one.
func BenchmarkReadIntersectCardinality(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			idx := NewIndex()
			buildLabel(idx, 1, 0, n)
			buildLabel(idx, 2, n/2, n)
			b.ReportAllocs()
			b.ResetTimer()
			var sink uint64
			for range b.N {
				c, _ := idx.IntersectCardinality(1, 2)
				sink += c
			}
			runtimeSink(sink)
		})
	}
}

func BenchmarkReadUnion(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			idx := NewIndex()
			buildLabel(idx, 1, 0, n)
			buildLabel(idx, 2, n/2, n)
			b.ReportAllocs()
			b.ResetTimer()
			var sink uint64
			for range b.N {
				sink += idx.Union(1, 2).GetCardinality()
			}
			runtimeSink(sink)
		})
	}
}

// BenchmarkReadParallelCount is the contended read: many goroutines reading the
// SAME label. It is the cell where a per-entry read lock could lose to one
// index-wide one, because both are then the same single lock.
func BenchmarkReadParallelCount(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			idx := NewIndex()
			buildLabel(idx, 1, 0, n)
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				var sink uint64
				for pb.Next() {
					sink += idx.Count(1)
				}
				runtimeSink(sink)
			})
		})
	}
}

// BenchmarkWriteAddHotLabel is the contended write on ONE label — the case the
// per-label geometry explicitly does NOT fix, since one label is still one
// lock. It is measured so the cost is a number rather than a caveat.
func BenchmarkWriteAddHotLabel(b *testing.B) {
	idx := NewIndex()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var k uint64
		for pb.Next() {
			idx.Add(1, graph.NodeID(k))
			k++
		}
	})
}

// BenchmarkWriteAddManyLabels is the contended write spread over 64 labels —
// the case the geometry exists for.
func BenchmarkWriteAddManyLabels(b *testing.B) {
	idx := NewIndex()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var k uint64
		for pb.Next() {
			idx.Add(uint32(k%64), graph.NodeID(k))
			k++
		}
	})
}

// runtimeSink keeps a benchmark's result live so the compiler cannot delete the
// work being measured.
//
//go:noinline
func runtimeSink(v uint64) { sinkVar = v }

var sinkVar uint64

// runtimeMemStats and its two helpers keep the heap-measurement plumbing in one
// place so the benchmark body reads as the measurement it is.
type runtimeMemStats = runtime.MemStats

func readMemStats(m *runtime.MemStats) {
	runtime.GC()
	runtime.ReadMemStats(m)
}

//go:noinline
func runtimeKeepAlive(i *Index) { keepAliveVar = i }

var keepAliveVar *Index

// BenchmarkLabelCreation measures what one more DISTINCT label costs, in time
// and in resident bytes.
//
// Both halves are load-bearing for rmp #2685. The time half is a guard against
// the copy-on-write spine that was measured and rejected: rebuilding the map on
// every creation made this O(N) per label and O(N^2) overall — 789,535ns at
// 64,000 labels against 66ns — and GoGraph materialises a label entry on the
// FIRST Add, which is a data-path event, not a DDL one. The bytes half is the
// cost the per-entry geometry actually charges: a lock and its cache-line
// padding per label, paid whether the label is hot or carries a single node.
func BenchmarkLabelCreation(b *testing.B) {
	for _, n := range []int{1000, 64_000} {
		b.Run(fmt.Sprintf("labels=%d", n), func(b *testing.B) {
			var before, after runtimeMemStats
			readMemStats(&before)
			b.ResetTimer()
			var idx *Index
			for range b.N {
				idx = NewIndex()
				for l := range n {
					idx.Add(uint32(l), graph.NodeID(l))
				}
			}
			b.StopTimer()
			readMemStats(&after)
			runtimeKeepAlive(idx)
			if b.N > 0 {
				grown := float64(after.HeapAlloc) - float64(before.HeapAlloc)
				b.ReportMetric(grown/float64(n), "B/label-resident")
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(n), "ns/label")
		})
	}
}
