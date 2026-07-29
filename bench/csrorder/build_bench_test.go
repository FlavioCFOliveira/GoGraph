package csrorder

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// build_bench_test.go — the COST side of the ordering, reported per degree.
//
// rmp #2145 requires that the build and checkpoint delta be reported, not hidden.
// The ordering moves the CSR build from O(V+E) to O(V + Σ d log d) and the
// checkpoint inherits it, so a benchmark that showed only the read-path win would
// be reporting half the ledger. #2143 already measured this cost at ~4.2 ms per
// build on the 960k-edge cypher_scale fixture and answered it with an
// Engine-level pair cache; these benchmarks make the underlying build cost itself
// visible and attributable, per degree.
//
// Unlike the query benchmarks, both arms live in ONE binary here: the unordered
// arm is reachable through exported API ([UnorderedArrays] reproduces passes 1-2
// of csr.BuildFromAdjList and stops before its pass 3), so no cross-commit run is
// needed and the delta is directly readable from a single benchstat table.

// BenchmarkCSRBuild_Ordered is the shipped build: passes 1-2 plus the ordering
// pass, exactly as the Cypher engine invokes it (live-filtered).
func BenchmarkCSRBuild_Ordered(b *testing.B) {
	for _, d := range SweptDegrees {
		f, err := HubFixture(d, probeThreshold)
		if err != nil {
			b.Fatalf("HubFixture(%d): %v", d, err)
		}
		adj := f.Graph.AdjList()
		live := f.Graph.LiveNodeFilter()
		b.Run(degreeName(d), func(b *testing.B) {
			reportProfile(b, f)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				c := csr.BuildFromAdjListLive(adj, live)
				if c.Size() == 0 {
					b.Fatal("built an empty CSR: fixture is wrong")
				}
			}
		})
	}
}

// BenchmarkCSRBuild_Unordered is the pre-#2141 build: the same two passes with no
// ordering. The delta against BenchmarkCSRBuild_Ordered at the same degree is the
// ordering's write-side price.
func BenchmarkCSRBuild_Unordered(b *testing.B) {
	for _, d := range SweptDegrees {
		f, err := HubFixture(d, probeThreshold)
		if err != nil {
			b.Fatalf("HubFixture(%d): %v", d, err)
		}
		adj := f.Graph.AdjList()
		b.Run(degreeName(d), func(b *testing.B) {
			reportProfile(b, f)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				vertices, edges, _, _ := UnorderedArrays(adj)
				if len(vertices) == 0 || len(edges) == 0 {
					b.Fatal("built empty arrays: fixture is wrong")
				}
			}
		})
	}
}

// BenchmarkOrderRuns isolates the ordering pass ALONE, on pre-built arrays.
//
// This is the number to quote for "what does ordering cost", because the two
// build benchmarks above each also pay the O(V+E) copy that dominates them. It is
// measured on a fresh copy per iteration: ordering is in-place and idempotent, so
// re-ordering an already-ordered array would take the cheap
// already-ordered path (order.go's runOrdered pre-check above the merge cutoff,
// and insertion sort's best case below it) and report a cost the real build never
// pays.
func BenchmarkOrderRuns(b *testing.B) {
	for _, d := range SweptDegrees {
		f, err := HubFixture(d, probeThreshold)
		if err != nil {
			b.Fatalf("HubFixture(%d): %v", d, err)
		}
		vertices, edges, weights, handles := UnorderedArrays(f.Graph.AdjList())
		b.Run(degreeName(d), func(b *testing.B) {
			reportProfile(b, f)
			b.ReportAllocs()
			// Scratch copies, refreshed per iteration inside a stopped timer so
			// the refresh is excluded from the measurement.
			//
			// A nil column must stay nil rather than become an empty slice:
			// OrderRuns decides whether a parallel column exists by comparing it
			// against nil, so handing it a zero-length non-nil slice would make it
			// permute a column that is not there.
			ce := make([]graph.NodeID, len(edges))
			var cw []float64
			if weights != nil {
				cw = make([]float64, len(weights))
			}
			var ch []uint64
			if handles != nil {
				ch = make([]uint64, len(handles))
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				copy(ce, edges)
				copy(cw, weights)
				copy(ch, handles)
				b.StartTimer()
				csr.OrderRuns(vertices, ce, cw, ch)
			}
		})
	}
}
