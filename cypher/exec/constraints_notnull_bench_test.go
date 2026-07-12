package exec

import (
	"strconv"
	"testing"
)

// constraints_notnull_bench_test.go — benchmark for #1911. NotNullProperties is
// on the commit-time existence-check hot path (called once per touched node ×
// label). Before the fix it RLocked, scanned the entire notNull map, split
// every key, and allocated a fresh slice per call; the label index makes it an
// O(1) lookup that returns the registry's copy-on-write slice with zero
// allocation. Run: go test -bench=BenchmarkNotNullProperties -benchmem ./cypher/exec/
func BenchmarkNotNullProperties(b *testing.B) {
	reg := NewConstraintRegistry()
	// Populate many NOT NULL constraints across distinct labels so the old
	// full-map scan cost (and per-call allocation) would scale with the set.
	for i := 0; i < 256; i++ {
		lbl := "Label" + strconv.Itoa(i)
		reg.RegisterNotNull(lbl, "prop")
	}
	reg.RegisterNotNull("Hot", "email")

	b.ReportAllocs()
	b.ResetTimer()
	var sink []string
	for i := 0; i < b.N; i++ {
		sink = reg.NotNullProperties("Hot")
	}
	_ = sink
}
