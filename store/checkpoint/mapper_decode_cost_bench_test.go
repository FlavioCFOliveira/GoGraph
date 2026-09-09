package checkpoint

// mapper_decode_cost_bench_test.go — the cost measurement rmp #2780 requires.
//
// rmp #2749 measured the PARSE it added to phase 2 at ~0.95 ms/MiB, about 10%
// of a checkpoint. rmp #2780 adds a codec-decode pass over the mapper readback
// that parse already produced. The claim is that it costs no extra I/O; that is
// a claim about I/O, not about time, so the CPU it does spend is measured here
// rather than assumed away.
//
// The fixture is int-keyed on purpose. snapshot.WriteMapper emits the frozen
// version-1 layout for N=string, whose keys carry no codec framing and land in
// MapperReadback.Pairs, so the decode pass is a no-op on a string store and a
// string benchmark would measure zero and prove nothing. int keys produce the
// version-2 (codec) layout — the only layout the decode pass applies to — so
// these figures are the pass doing its full work.
//
// Two benchmarks, to be read together and run INTERLEAVED (-count=N alternates
// them), compared with benchstat:
//
//   - BenchmarkCheckpoint_CodecReadback_ParseOnly times snapshot.LoadSnapshotFull
//     alone: the rmp #2749 readback, on this image.
//   - BenchmarkCheckpoint_CodecReadback_ParseAndDecode times the shipped
//     VerifySnapshotReadable: the same parse PLUS the rmp #2780 decode pass.
//
// The difference between them is what rmp #2780 costs. Both report snapshotMiB
// and mapperPairs so the figures can be normalised and re-derived at another
// scale.

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// decodeBenchGraph builds the int-keyed twin of gateBenchGraph: the same 50k
// nodes / 500k edges, the same labels and properties, so the parse half of the
// measurement is comparable with the rmp #2749 figures. Only the key type
// differs, which is what switches mapper.bin to the version-2 codec layout.
func decodeBenchGraph(tb testing.TB) *lpg.Graph[int, int64] {
	tb.Helper()
	g := lpg.New[int, int64](adjlist.Config{Directed: true})
	for i := 0; i < gateBenchNodes; i++ {
		if err := g.AddNode(i); err != nil {
			tb.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(i, "Person"); err != nil {
			tb.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(i, "name", lpg.StringValue("n"+itoaBench(i))); err != nil {
			tb.Fatalf("SetNodeProperty: %v", err)
		}
		if err := g.SetNodeProperty(i, "seq", lpg.Int64Value(int64(i))); err != nil {
			tb.Fatalf("SetNodeProperty: %v", err)
		}
	}
	for i := 0; i < gateBenchNodes; i++ {
		for d := 1; d <= gateBenchDegree; d++ {
			if err := g.AddEdge(i, (i*7919+d*13)%gateBenchNodes, int64(d)); err != nil {
				tb.Fatalf("AddEdge: %v", err)
			}
		}
	}
	return g
}

// itoaBench is strconv.Itoa under a local name, so this file does not have to
// import strconv solely to keep the property payload byte-comparable with
// gateBenchGraph's.
func itoaBench(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

// decodeBenchPublish publishes one snapshot of the int-keyed fixture and returns
// its directory, its on-disk size and the number of version-2 mapper records it
// carries. It fails the benchmark if the published mapper is NOT the codec
// layout, because the decode pass would then be timed doing nothing.
func decodeBenchPublish(tb testing.TB) (snapDir string, snapBytes int64, pairs int) {
	tb.Helper()
	dir := tb.TempDir()
	g := decodeBenchGraph(tb)
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		tb.Fatalf("wal.Open: %v", err)
	}
	tb.Cleanup(func() { _ = w.Close() })
	var mu sync.Mutex
	cp := New[int, int64](Config{Dir: dir}, g, w, &mu,
		WithMapperCodec[int, int64](txn.NewIntCodec()),
		WithWeightCodec[int, int64](txn.NewInt64WeightCodec()),
	)
	if err := cp.RunCheckpoint(); err != nil {
		tb.Fatalf("RunCheckpoint: %v", err)
	}
	snapDir = filepath.Join(dir, "snapshot")
	snapBytes = gateBenchSnapshotBytes(tb, snapDir)

	loaded, err := snapshot.LoadSnapshotFull(snapDir)
	if err != nil {
		tb.Fatalf("LoadSnapshotFull: %v", err)
	}
	if len(loaded.Mapper.RawPairs) == 0 {
		tb.Fatalf("published mapper carries no RawPairs (Pairs=%d): the fixture did not "+
			"produce the version-2 codec layout, so the decode pass would be timed doing "+
			"nothing", len(loaded.Mapper.Pairs))
	}
	return snapDir, snapBytes, len(loaded.Mapper.RawPairs)
}

// BenchmarkCheckpoint_CodecReadback_ParseOnly times the rmp #2749 half alone on
// a version-2 image: snapshot.LoadSnapshotFull, no decode pass. It is the
// baseline the shipped readback is compared against.
func BenchmarkCheckpoint_CodecReadback_ParseOnly(b *testing.B) {
	snapDir, snapBytes, pairs := decodeBenchPublish(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := snapshot.LoadSnapshotFull(snapDir); err != nil {
			b.Fatalf("LoadSnapshotFull: %v", err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(snapBytes)/(1<<20), "snapshotMiB")
	b.ReportMetric(float64(pairs), "mapperPairs")
}

// BenchmarkCheckpoint_CodecReadback_ParseAndDecode times the shipped phase-2
// readback on the same image: the identical parse PLUS the rmp #2780
// codec-decode pass over the mapper readback it produced. Subtract the
// benchmark above to obtain what rmp #2780 costs.
func BenchmarkCheckpoint_CodecReadback_ParseAndDecode(b *testing.B) {
	snapDir, snapBytes, pairs := decodeBenchPublish(b)
	backend := osSnapshotBackend[int, int64]{}
	codec := txn.NewIntCodec()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := backend.VerifySnapshotReadable(snapDir, codec); err != nil {
			b.Fatalf("VerifySnapshotReadable: %v", err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(snapBytes)/(1<<20), "snapshotMiB")
	b.ReportMetric(float64(pairs), "mapperPairs")
}

// BenchmarkCheckpoint_MapperDecodeOnly times snapshot.VerifyMapperDecodable in
// isolation, on a mapper readback already in memory. It carries no filesystem
// call at all, which is the direct evidence for the "zero extra I/O" half of the
// rmp #2780 claim: whatever this costs is CPU, and it is the whole of what the
// shipped readback adds on top of the parse.
func BenchmarkCheckpoint_MapperDecodeOnly(b *testing.B) {
	snapDir, _, pairs := decodeBenchPublish(b)
	loaded, err := snapshot.LoadSnapshotFull(snapDir)
	if err != nil {
		b.Fatalf("LoadSnapshotFull: %v", err)
	}
	codec := txn.NewIntCodec()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := snapshot.VerifyMapperDecodable[int](loaded.Mapper, codec); err != nil {
			b.Fatalf("VerifyMapperDecodable: %v", err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(pairs), "mapperPairs")
}
