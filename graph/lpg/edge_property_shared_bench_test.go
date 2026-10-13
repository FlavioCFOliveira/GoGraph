package lpg

// edge_property_shared_bench_test.go — rmp #3090.
//
// One op builds one hub of the given out-degree through the public fused
// append, [Graph.AddEdgeLabeledWithProperty], alternating two property keys
// per edge (two columns per source block, the shape examples/26 loads). It is
// the graph-level counterpart of BenchmarkFusedPack_Fused: B/op and allocs/op
// include the adjacency entry, the edge-property block and node creation.
//
//	go test -run x -bench BenchmarkFusedEdgeInsert -benchmem -count=10 ./graph/lpg/
//
// Layer: short (bench-only).

import (
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func BenchmarkFusedEdgeInsert(b *testing.B) {
	for _, degree := range []int{64, 1024} {
		names := make([]string, degree)
		for i := range names {
			names[i] = "t" + strconv.Itoa(i)
		}
		b.Run("deg="+strconv.Itoa(degree), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				g := New[string, float64](adjlist.Config{})
				for j, dst := range names {
					key := "since"
					if j&1 == 1 {
						key = "weight"
					}
					if err := g.AddEdgeLabeledWithProperty("h", dst, 1, "KNOWS", key, Int64Value(int64(j))); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
