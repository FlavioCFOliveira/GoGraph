package btree

// concurrency_bench_test.go — the scaling guard for the copy-on-write,
// lock-free-read redesign (task #2683).
//
// The redesign exists for one measured reason: with a single RWMutex the index
// ANTI-scaled. RLock/RUnlock are atomic read-modify-writes on one cache line,
// so a PURE-READ workload collapsed as soon as a second core joined, and the
// global write Lock decayed it further at high goroutine counts.
//
// These benchmarks are the permanent evidence for that, and the tripwire
// against a future change that reintroduces a shared word on the read path.
// Read them as a RATIO across -cpu levels, never as an absolute:
//
//	go test -run x -bench 'Concurrent' -benchmem -count=10 \
//	    -cpu 1,8,64,256,1024 ./graph/index/btree/ > new.txt
//	benchstat old.txt new.txt
//
// A healthy read path shows ns/op roughly FLAT or falling as -cpu rises (the
// b.RunParallel harness divides the wall time by the total op count, so perfect
// scaling is flat); a shared-cache-line read path shows it climbing.

import (
	"math/rand/v2"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// benchIndexKeys is the size of the index the concurrency benchmarks read. It
// is large enough that the tree is three levels deep, so a descent is a real
// pointer chase rather than a single cache-resident leaf.
const benchIndexKeys = 1 << 20

// buildBenchIndex returns a bulk-loaded index of benchIndexKeys sequential
// int64 keys, one node id each.
func buildBenchIndex(tb testing.TB) *Index[int64] {
	tb.Helper()
	vals := make([]int64, benchIndexKeys)
	nodes := make([]graph.NodeID, benchIndexKeys)
	for i := range vals {
		vals[i] = int64(i)
		nodes[i] = graph.NodeID(uint64(i))
	}
	idx := New[int64]()
	if err := idx.BulkLoadSorted(vals, nodes); err != nil {
		tb.Fatalf("BulkLoadSorted: %v", err)
	}
	return idx
}

// BenchmarkConcurrentLookup is the pure-read scaling probe: every goroutine
// does point lookups over the whole key space and nothing writes. Any rise in
// ns/op with -cpu is contention the read path should not have.
func BenchmarkConcurrentLookup(b *testing.B) {
	idx := buildBenchIndex(b)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic bench RNG
		for pb.Next() {
			_ = idx.Cardinality(int64(r.IntN(benchIndexKeys)))
		}
	})
}

// BenchmarkConcurrentRangeFirst is the pure-read scaling probe for the cursor
// descent, which replaced the leaf chain.
func BenchmarkConcurrentRangeFirst(b *testing.B) {
	idx := buildBenchIndex(b)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		r := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // deterministic bench RNG
		for pb.Next() {
			lo := int64(r.IntN(benchIndexKeys))
			_, _, _ = idx.RangeFirst(lo, lo+64)
		}
	})
}

// BenchmarkConcurrentMixed90Read is the 90/10 read/write mix. The writes are
// deliberately NON-structural — they add and remove node ids under keys that
// already exist — because that is the shape the redesign makes lock-free, and
// the shape a live index actually sees between schema changes.
func BenchmarkConcurrentMixed90Read(b *testing.B) {
	idx := buildBenchIndex(b)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		r := rand.New(rand.NewPCG(5, 6)) //nolint:gosec // deterministic bench RNG
		// Node ids above the loaded population, so a write never empties a key
		// and the mix stays at its declared shape instead of drifting into a
		// structural-delete workload.
		var writeID uint64 = benchIndexKeys
		for pb.Next() {
			k := int64(r.IntN(benchIndexKeys))
			if r.IntN(10) == 0 {
				writeID++
				idx.Insert(k, graph.NodeID(writeID))
				idx.Delete(k, graph.NodeID(writeID))
				continue
			}
			_ = idx.Cardinality(k)
		}
	})
}

// BenchmarkConcurrentStructuralInsert measures the price the redesign pays:
// creating a distinct key copies the root-to-leaf path instead of shifting one
// slice in place, and every such write serialises on the one structural mutex.
// It is here so that cost stays visible and bounded, not because it is
// expected to scale.
func BenchmarkConcurrentStructuralInsert(b *testing.B) {
	idx := New[int64]()
	// One shared key counter, so every goroutine creates DISTINCT keys against
	// the SAME index and the benchmark measures the structural mutex rather
	// than a private tree per worker.
	var next atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			k := next.Add(1)
			idx.Insert(k, graph.NodeID(uint64(k)))
		}
	})
}
