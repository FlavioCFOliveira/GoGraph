package checkpoint

// readback_cost_bench_test.go — the cost measurement rmp #2749 requires.
//
// The fix parses the whole published snapshot before the WAL prefix is
// discarded. That is real I/O on the checkpoint path, so its cost is measured
// rather than asserted, at the scale the project already treats as
// representative for snapshot work (50k nodes / 500k edges — the same fixture
// size as store/recovery/snapshot_apply_bench_test.go).
//
// Two benchmarks, to be read together:
//
//   - BenchmarkCheckpoint_FullRunWithReadback times a whole RunCheckpoint on
//     the CURRENT code, i.e. capture + publish + readback + truncate.
//   - BenchmarkCheckpoint_SnapshotReadbackOnly times the readback alone, on the
//     snapshot that same checkpoint publishes.
//
// The second is the cost the fix ADDS; the first is what it is added to. Run
// them interleaved (-count=N runs each benchmark N times, alternating) and
// compare with benchstat.
//
// Both report snapshotMiB so the numbers can be normalised to the image size
// and re-derived at another scale.

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

const (
	// gateBenchNodes matches store/recovery/snapshot_apply_bench_test.go's
	// fixture so the figures are comparable with the project's existing
	// snapshot measurements.
	gateBenchNodes = 50_000
	// gateBenchDegree gives 500k edges, the same edge count as that fixture.
	gateBenchDegree = 10
)

// gateBenchGraph builds the fixture graph directly (no WAL): the checkpoint
// still captures and serialises every component, which is the cost under study.
// Every component the reader parses is populated — adjacency, mapper, labels
// and properties — so the readback is not measured on a degenerate image that
// happens to skip most of its work.
func gateBenchGraph(tb testing.TB) *lpg.Graph[string, int64] {
	tb.Helper()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	for i := 0; i < gateBenchNodes; i++ {
		k := "n" + strconv.Itoa(i)
		if err := g.AddNode(k); err != nil {
			tb.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "Person"); err != nil {
			tb.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(k, "name", lpg.StringValue(k)); err != nil {
			tb.Fatalf("SetNodeProperty: %v", err)
		}
		if err := g.SetNodeProperty(k, "seq", lpg.Int64Value(int64(i))); err != nil {
			tb.Fatalf("SetNodeProperty: %v", err)
		}
	}
	for i := 0; i < gateBenchNodes; i++ {
		src := "n" + strconv.Itoa(i)
		for d := 1; d <= gateBenchDegree; d++ {
			dst := "n" + strconv.Itoa((i*7919+d*13)%gateBenchNodes)
			if err := g.AddEdge(src, dst, int64(d)); err != nil {
				tb.Fatalf("AddEdge: %v", err)
			}
		}
	}
	return g
}

// gateBenchCheckpointer wires a checkpointer over the fixture graph in dir.
func gateBenchCheckpointer(tb testing.TB, dir string, g *lpg.Graph[string, int64]) *Checkpointer[string, int64] {
	tb.Helper()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		tb.Fatalf("wal.Open: %v", err)
	}
	tb.Cleanup(func() { _ = w.Close() })
	var mu sync.Mutex
	return New[string, int64](Config{Dir: dir}, g, w, &mu,
		WithMapperCodec[string, int64](txn.NewStringCodec()),
		WithWeightCodec[string, int64](txn.NewInt64WeightCodec()),
	)
}

// gateBenchSnapshotBytes sums the on-disk size of every file in the snapshot
// directory, so each benchmark can report the image size its figures describe.
func gateBenchSnapshotBytes(tb testing.TB, snapDir string) int64 {
	tb.Helper()
	var total int64
	err := filepath.Walk(snapDir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		tb.Fatalf("walk snapshot dir: %v", err)
	}
	if total == 0 {
		tb.Fatal("snapshot directory is empty: the benchmark would measure nothing")
	}
	return total
}

// BenchmarkCheckpoint_FullRunWithReadback times one complete checkpoint on the
// current code — phase 1 capture, phase 2 publish AND readback, phase 3
// truncate. It is the denominator for the readback's relative cost.
func BenchmarkCheckpoint_FullRunWithReadback(b *testing.B) {
	dir := b.TempDir()
	g := gateBenchGraph(b)
	cp := gateBenchCheckpointer(b, dir, g)

	// One warm-up checkpoint so the snapshot directory exists and its size can
	// be reported, and so the first timed iteration is not the only one paying
	// directory creation.
	if err := cp.RunCheckpoint(); err != nil {
		b.Fatalf("warm-up RunCheckpoint: %v", err)
	}
	snapBytes := gateBenchSnapshotBytes(b, filepath.Join(dir, "snapshot"))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := cp.RunCheckpoint(); err != nil {
			b.Fatalf("RunCheckpoint: %v", err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(snapBytes)/(1<<20), "snapshotMiB")
}

// BenchmarkCheckpoint_SnapshotReadbackOnly times ONLY the verification the fix
// adds, on the very snapshot the checkpoint above publishes. This is the added
// cost, in isolation.
func BenchmarkCheckpoint_SnapshotReadbackOnly(b *testing.B) {
	dir := b.TempDir()
	g := gateBenchGraph(b)
	cp := gateBenchCheckpointer(b, dir, g)
	if err := cp.RunCheckpoint(); err != nil {
		b.Fatalf("RunCheckpoint: %v", err)
	}
	snapDir := filepath.Join(dir, "snapshot")
	snapBytes := gateBenchSnapshotBytes(b, snapDir)

	backend := osSnapshotBackend[string, int64]{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := backend.VerifySnapshotReadable(snapDir); err != nil {
			b.Fatalf("VerifySnapshotReadable: %v", err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(snapBytes)/(1<<20), "snapshotMiB")
}
