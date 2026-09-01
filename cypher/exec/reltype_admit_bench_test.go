package exec_test

// reltype_admit_bench_test.go — the PRIMITIVE cost of a per-slot relationship-type
// test, measured in place (rmp #2251).
//
// It exists so the change's headline can never be quoted as a primitive ratio, nor
// the primitive ratio as an end-to-end result. The two differ by an order of
// magnitude and both are reported: this benchmark says what ONE type test costs,
// and docs/benchmarks/reltype-column-2026-08-29.md says what a query costs.
//
// The map arm reproduces exactly what [Expand.passesFilter] did before the change
// — a membership probe into a map[uint64]string keyed by absolute forward CSR
// position, holding one entry per ACCEPTED arc across the whole graph. The column
// arm is what it does now. Both walk the same pseudo-random position sequence, so
// neither gets a sequential-access advantage the real traversal would not have.

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// relTypeProbeArcs is large enough that the map cannot live in L2, which is the
// regime a real graph presents. A map that fits in cache flatters the arm being
// replaced.
const relTypeProbeArcs = 1 << 20

// relTypeProbePositions is the access sequence, shared by both arms.
func relTypeProbePositions() []uint64 {
	pos := make([]uint64, relTypeProbeArcs)
	// A large odd stride: every position is visited exactly once, in an order that
	// defeats prefetching, without needing an RNG in the timed loop.
	const stride = 104729
	for i := range pos {
		pos[i] = uint64((i * stride) % relTypeProbeArcs)
	}
	return pos
}

// BenchmarkRelTypeProbe_Map is the retired per-slot map probe.
func BenchmarkRelTypeProbe_Map(b *testing.B) {
	filter := make(map[uint64]string, relTypeProbeArcs/2)
	for i := 0; i < relTypeProbeArcs; i += 2 { // half the arcs accepted
		filter[uint64(i)] = "K"
	}
	pos := relTypeProbePositions()
	b.ReportAllocs()
	b.ResetTimer()
	hits := 0
	for i := 0; i < b.N; i++ {
		if _, ok := filter[pos[i%len(pos)]]; ok {
			hits++
		}
	}
	b.StopTimer()
	if hits == 0 {
		b.Fatal("no probe hit, so the timed loop was not doing the work under test")
	}
}

// BenchmarkRelTypeProbe_Column is the slot-aligned column's indexed load plus bit
// test, over the identical accepted set and the identical access sequence.
func BenchmarkRelTypeProbe_Column(b *testing.B) {
	codes := make([]uint32, relTypeProbeArcs)
	for i := 0; i < relTypeProbeArcs; i += 2 {
		codes[i] = 1 // encoded LabelID 0
	}
	admit := exec.NewRelTypeColumn(codes, nil, nil, nil).Admit([]uint32{1})
	pos := relTypeProbePositions()
	b.ReportAllocs()
	b.ResetTimer()
	hits := 0
	for i := 0; i < b.N; i++ {
		if admit.Fwd(pos[i%len(pos)]) {
			hits++
		}
	}
	b.StopTimer()
	if hits == 0 {
		b.Fatal("no probe hit, so the timed loop was not doing the work under test")
	}
}

// BenchmarkRelTypeProbe_ReverseRecovery is the REVERSE side's real difference, and
// the larger of the two: before the change a reverse slot had to RECOVER its
// forward position before it could be probed at all. The recovery is charged here
// against the column's single indexed load.
func BenchmarkRelTypeProbe_ReverseRecovery(b *testing.B) {
	// A CSR whose every source has `degree` neighbours, so the recovery's binary
	// search has a run of that length to land in.
	const degree = 64
	const sources = relTypeProbeArcs / degree
	edges := make([][2]int, 0, relTypeProbeArcs)
	for s := 0; s < sources; s++ {
		for d := 0; d < degree; d++ {
			edges = append(edges, [2]int{s, (s + d + 1) % sources})
		}
	}
	fwd := buildCSR(sources, edges)
	fwdVerts, fwdEdges := fwd.VerticesSlice(), fwd.EdgesSlice()

	filter := make(map[uint64]string, len(fwdEdges)/2)
	for i := 0; i < len(fwdEdges); i += 2 {
		filter[uint64(i)] = "K"
	}
	codes := make([]uint32, len(fwdEdges))
	for i := 0; i < len(fwdEdges); i += 2 {
		codes[i] = 1
	}
	admit := exec.NewRelTypeColumn(codes, codes, nil, nil).Admit([]uint32{1})

	b.Run("map+recovery", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		hits := 0
		for i := 0; i < b.N; i++ {
			src := uint64(i % sources)
			dst := uint64((i*7 + 1) % sources)
			fp, ok := exec.FirstDstPosForTest(fwdEdges, fwdVerts[src], fwdVerts[src+1], dst)
			if !ok {
				continue
			}
			if _, in := filter[fp]; in {
				hits++
			}
		}
		b.StopTimer()
		if hits == 0 {
			b.Fatal("no probe hit, so the timed loop was not doing the work under test")
		}
	})

	b.Run("column", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		hits := 0
		for i := 0; i < b.N; i++ {
			if ok, known := admit.Rev(uint64(i) % uint64(len(fwdEdges))); known && ok {
				hits++
			}
		}
		b.StopTimer()
		if hits == 0 {
			b.Fatal("no probe hit, so the timed loop was not doing the work under test")
		}
	})
	_ = fmt.Sprint
}
