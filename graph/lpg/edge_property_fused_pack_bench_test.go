package lpg

// edge_property_fused_pack_bench_test.go — rmp #2171.
//
// Records the footprint of a frozen edge-property date column on both build
// paths, at the two degrees the round-3 audit measured. Before Compact reshaped,
// the fused path never packed: its column stayed in the sparse coordinate-list
// form, which maybePackDate declines outright.
//
//	go test -run x -bench BenchmarkFusedPack -benchmem -count=6 ./graph/lpg/
//
// B/op here is the ALLOCATION cost of building and freezing one column, not its
// resident size; the resident bytes-per-edge figure — the number the audit
// quoted and the one this task is judged on — is asserted deterministically in
// TestFusedPack_FusedMatchesSetAfter and reported by these benchmarks through
// ReportMetric as "B/edge", computed from the frozen column's actual physical
// planes rather than sampled from the heap.
//
// Layer: short (bench-only).

import "testing"

// fusedPackDegrees are the degrees the audit measured: 30.5% waste at 324 and
// 24.3% at 64.
var fusedPackDegrees = []int{64, 324}

// frozenColumnBytes builds a column by one of the two paths, freezes it, and
// returns its physical footprint in bytes.
func frozenColumnBytes(tb testing.TB, degree int, fused bool) int {
	const key = PropertyKeyID(21)
	days := benchDays(degree)

	var block *edgePropCols
	if fused {
		first, ok := newEdgePropColsAux(1, &edgePropPayload{keyID: key, value: dateVal(days[0])}).(*edgePropCols)
		if !ok || first == nil {
			tb.Fatal("newEdgePropColsAux returned no block")
		}
		block = first
		for i := 1; i < degree; i++ {
			next, ok := block.GrowSlotWithValue(i, &edgePropPayload{keyID: key, value: dateVal(days[i])}).(*edgePropCols)
			if !ok || next == nil {
				tb.Fatalf("GrowSlotWithValue returned no block at slot %d", i)
			}
			block = next
		}
	} else {
		for i, ed := range days {
			block = block.set(key, i, i+1, dateVal(ed))
		}
	}

	frozen, ok := block.Compact().(*edgePropCols)
	if !ok || frozen == nil {
		tb.Fatal("Compact returned no block")
	}
	for i := range frozen.cols {
		if frozen.cols[i].key == key {
			return colPhysicalBytes(&frozen.cols[i])
		}
	}
	tb.Fatalf("no column for key %d after Compact", key)
	return 0
}

// benchFusedPack drives build-and-freeze b.N times and reports the frozen
// column's resident bytes per edge alongside the usual allocation metrics.
func benchFusedPack(b *testing.B, fused bool) {
	for _, degree := range fusedPackDegrees {
		b.Run(itoa(degree), func(b *testing.B) {
			// Measure the footprint once, outside the timed loop, and report it as
			// a custom metric so benchstat tracks it across the change.
			perEdge := float64(frozenColumnBytes(b, degree, fused)) / float64(degree)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = frozenColumnBytes(b, degree, fused)
			}
			b.StopTimer()
			b.ReportMetric(perEdge, "B/edge")
		})
	}
}

// BenchmarkFusedPack_Fused measures the fused append path — the one
// examples/26_social_scale_bench drives, and the one that never packed.
func BenchmarkFusedPack_Fused(b *testing.B) { benchFusedPack(b, true) }

// BenchmarkFusedPack_SetAfter measures the set-after path, which packed all
// along and is the reference the fused path had to reach.
func BenchmarkFusedPack_SetAfter(b *testing.B) { benchFusedPack(b, false) }
