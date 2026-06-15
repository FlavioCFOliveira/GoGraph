package btree

import (
	"math/rand/v2"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// bench_test.go — the insert/delete win and the read-path no-regression
// guard for task #1514 (sorted-array → B+ tree). Capture the sorted-array
// numbers, then re-run after the swap and compare with benchstat.
//
//	go test -run x -bench 'Insert|Delete|Lookup|Range|RangeCount|Bulk' \
//	    -benchmem -count=10 ./graph/index/btree/ > old.txt   # before
//	# ... swap implementation ...
//	go test -run x -bench 'Insert|Delete|Lookup|Range|RangeCount|Bulk' \
//	    -benchmem -count=10 ./graph/index/btree/ > new.txt   # after
//	benchstat old.txt new.txt
//
// The Insert/Delete benchmarks drive NEW DISTINCT KEYS — the O(n) path on
// the sorted array (array shift) that the B+ tree turns into O(log n).

// benchKeys returns a deterministic random permutation of [0,n) as int64
// keys so inserts hit random positions (worst case for the array shift).
func benchKeys(n int) []int64 {
	r := rand.New(rand.NewPCG(0x1514, 0xB7EE)) //nolint:gosec // deterministic bench RNG
	keys := make([]int64, n)
	for i := range keys {
		keys[i] = int64(i)
	}
	r.Shuffle(n, func(a, b int) { keys[a], keys[b] = keys[b], keys[a] })
	return keys
}

// BenchmarkIndex_InsertDistinct_100k inserts 100k distinct keys in random
// order — the core write-heavy path. On the sorted array each new key costs
// an O(n) shift; on the B+ tree it is O(log n).
func BenchmarkIndex_InsertDistinct_100k(b *testing.B) {
	const n = 100_000
	keys := benchKeys(n)
	b.ReportAllocs()
	b.ResetTimer()
	for iter := 0; iter < b.N; iter++ {
		idx := New[int64]()
		for k := 0; k < n; k++ {
			idx.Insert(keys[k], graph.NodeID(uint64(keys[k])))
		}
	}
}

// BenchmarkIndex_DeleteDistinct_100k bulk-loads 100k distinct keys then
// deletes them all in random order — each delete empties a bitmap and
// removes the key (the O(n) array-shift path on the sorted array).
func BenchmarkIndex_DeleteDistinct_100k(b *testing.B) {
	const n = 100_000
	keys := benchKeys(n)
	vals := make([]int64, n)
	nodes := make([]graph.NodeID, n)
	for i := range keys {
		vals[i] = keys[i]
		nodes[i] = graph.NodeID(uint64(keys[i]))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iter := 0; iter < b.N; iter++ {
		b.StopTimer()
		idx := New[int64]()
		if err := idx.BulkLoad(vals, nodes); err != nil {
			b.Fatalf("BulkLoad: %v", err)
		}
		b.StartTimer()
		for k := 0; k < n; k++ {
			idx.Delete(keys[k], graph.NodeID(uint64(keys[k])))
		}
	}
}

// BenchmarkIndex_LookupHot probes the point-read hot path (no-regression
// guard): 1M sequential keys, random point lookups.
func BenchmarkIndex_LookupHot(b *testing.B) {
	const n = 1_000_000
	idx := buildSeq(b, n)
	r := rand.New(rand.NewPCG(7, 7)) //nolint:gosec // deterministic bench RNG
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = idx.Lookup(int64(r.IntN(n)))
	}
}

// BenchmarkIndex_RangeScan probes a moderately selective range union
// (no-regression guard).
func BenchmarkIndex_RangeScan(b *testing.B) {
	const n = 1_000_000
	idx := buildSeq(b, n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = idx.Range(250_000, 260_000)
	}
}

// BenchmarkIndex_RangeCountGate probes the #1505 selectivity gate path
// (no-regression guard): a non-selective range that early-exits on budget.
func BenchmarkIndex_RangeCountGate(b *testing.B) {
	const n = 1_000_000
	idx := buildSeq(b, n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = idx.RangeCount(0, int64(n), 1000)
	}
}

// buildSeq returns an index of n sequential int64 keys via BulkLoad.
func buildSeq(b *testing.B, n int) *Index[int64] {
	b.Helper()
	vals := make([]int64, n)
	nodes := make([]graph.NodeID, n)
	for i := range vals {
		vals[i] = int64(i)
		nodes[i] = graph.NodeID(uint64(i))
	}
	idx := New[int64]()
	if err := idx.BulkLoad(vals, nodes); err != nil {
		b.Fatalf("BulkLoad: %v", err)
	}
	return idx
}
