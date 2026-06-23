package index

import "testing"

// nodeset_bench_test.go — hot-path microbenchmarks for the packed NodeSet
// (#1596). They guard against the tagged-union read path losing on CPU what
// it wins on RAM: a tag branch per access must not measurably regress
// Contains / build throughput versus the predecessor union.

// BenchmarkNodeSet_BuildSingleton measures the dominant high-cardinality shape:
// building 1024 singleton sets (one Add each). This is the per-key cost on a
// unique-property index.
func BenchmarkNodeSet_BuildSingleton(b *testing.B) {
	b.ReportAllocs()
	sets := make([]NodeSet, 1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := range sets {
			sets[j] = NodeSet{}
			sets[j].Add(uint64(j))
		}
	}
	_ = sets
}

// BenchmarkNodeSet_ContainsSingleton measures membership probing on the
// singleton tier — the cheapest, most common read.
func BenchmarkNodeSet_ContainsSingleton(b *testing.B) {
	var s NodeSet
	s.Add(42)
	b.ResetTimer()
	var hit int
	for i := 0; i < b.N; i++ {
		if s.Contains(uint64(i & 63)) {
			hit++
		}
	}
	_ = hit
}

// BenchmarkNodeSet_ContainsSmall measures membership probing on the small
// (sorted-array) tier.
func BenchmarkNodeSet_ContainsSmall(b *testing.B) {
	var s NodeSet
	for _, v := range []uint64{2, 4, 6, 8, 10, 12, 14, 16} {
		s.Add(v)
	}
	b.ResetTimer()
	var hit int
	for i := 0; i < b.N; i++ {
		if s.Contains(uint64(i & 31)) {
			hit++
		}
	}
	_ = hit
}

// BenchmarkNodeSet_BuildSmall measures growing a set through the singleton ->
// small transitions up to smallSetMax, exercising the copy-on-write inserts.
func BenchmarkNodeSet_BuildSmall(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var s NodeSet
		for v := uint64(0); v < smallSetMax; v++ {
			s.Add(v * 2)
		}
		_ = s
	}
}
