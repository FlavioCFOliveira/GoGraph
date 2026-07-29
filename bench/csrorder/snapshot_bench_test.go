package csrorder

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
)

// snapshot_bench_test.go — the checkpoint's cost across the degree sweep.
//
// This benchmark is deliberately kept in its OWN file, separate from the CSR
// build benchmarks, because it must compile against the PRE-SPRINT tree as well
// as this one. rmp #2145 requires the checkpoint delta to be reported rather than
// hidden, and the checkpoint delta can only be obtained by a cross-commit
// differential: unlike the CSR build, whose unordered arm is reachable in-binary
// through [UnorderedArrays], there is no way to ask the snapshot writer for its
// pre-#2141 behaviour. So this file restricts itself to API that predates the
// sprint — snapshot.WriteSnapshotFull and csr.BuildFromAdjListLive via
// [OrderedCSR] — while build_bench_test.go is free to use csr.OrderRuns and so
// cannot be built at the baseline.

// BenchmarkSnapshotWrite measures the checkpoint's core work: a full snapshot
// write of the graph plus its ordered CSR.
//
// The checkpoint is where #2141 changed more than timing. Its label and
// edge-property collectors used to walk the adjacency in INSERTION order, which
// no longer matches the order a recovery replay produces, so both now emit
// canonically by sorted destination. That canonicalisation is inside this
// measurement, which is why the checkpoint is benchmarked rather than assumed to
// track the CSR build.
//
// The snapshot is written to b.TempDir(), which the testing package removes on
// completion, so a long -count run does not accumulate gigabytes on disk.
func BenchmarkSnapshotWrite(b *testing.B) {
	for _, d := range SweptDegrees {
		f, err := HubFixture(d, probeThreshold)
		if err != nil {
			b.Fatalf("HubFixture(%d): %v", d, err)
		}
		c := OrderedCSR(f.Graph)
		b.Run(degreeName(d), func(b *testing.B) {
			reportProfile(b, f)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				dir := b.TempDir()
				b.StartTimer()
				if err := snapshot.WriteSnapshotFull(dir, c, f.Graph); err != nil {
					b.Fatalf("WriteSnapshotFull: %v", err)
				}
			}
		})
	}
}
