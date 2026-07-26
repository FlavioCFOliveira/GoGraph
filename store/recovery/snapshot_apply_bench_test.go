package recovery

// snapshot_apply_bench_test.go — rmp #2170.
//
// The snapshot-apply path did not bracket itself in an adjacency commit window,
// so every edge restored from a snapshot cloned the touched shard's entire slot
// array (adjlist.storeEntry first-touch path). WAL replay has bracketed itself
// since task #1526; the snapshot path was simply missed. The consequence was
// that a checkpoint bought nothing: recovering from a snapshot cost about the
// same as replaying the whole WAL, which is the opposite of the point of taking
// one.
//
// Two benchmarks, over the same committed graph:
//
//	Snapshot — checkpoint taken (WAL truncated), so recovery is snapshot-only.
//	WALReplay — no checkpoint, so recovery replays every WAL frame.
//
// Run:
//
//	go test -run x -bench BenchmarkSnapshotApply -benchmem -count=6 ./store/recovery/
//
// Snapshot must be measurably FASTER than WALReplay — the acceptance criterion
// for #2170. Measured on an Apple M4 over 50k nodes and 500k edges, benchstat
// with 6 samples:
//
//	                  before          after           delta
//	Snapshot          147.45 ms       73.57 ms        -50.1%  (p=0.002)
//	                  737.6 MiB       113.6 MiB       -84.6%
//	WALReplay         176.6 ms        177.7 ms        +0.6%   (control)
//
// So a checkpoint went from buying 1.20x over a full replay to buying 2.42x.
// WALReplay is the control: it must not move, since the window it already had
// is untouched.
//
// The ratio scales with slots-per-shard, because that is what each avoided
// clone copies — a denser graph shows a larger factor than this fixture does.
//
// Layer: short (bench-only; benchmarks do not run under `go test`), and skipped
// under -short because the fixture write plus checkpoint is slow.

import (
	"context"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// These size the fixture. Each avoided clone copies one shard's whole slot
// array, so the effect needs a graph wide enough to give the shards real slot
// arrays, while keeping the one-off fixture write affordable.
const (
	snapshotApplyBenchNodes = 50_000
	snapshotApplyBenchEdges = 500_000
)

// buildRecoveryFixture writes a committed graph of the configured size into a
// fresh directory and returns it. When checkpointed is true it also takes a
// checkpoint, which writes a self-sufficient snapshot and truncates the WAL, so
// a subsequent recovery is snapshot-only.
func buildRecoveryFixture(tb testing.TB, checkpointed bool) string {
	tb.Helper()
	dir := tb.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		tb.Fatalf("wal.Open: %v", err)
	}

	g := lpg.New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	store := txn.NewStoreWithOptions[string, int64](g, w, opts)

	// One transaction for the nodes, then batches of edges. Batching keeps the
	// WAL frame count representative of real use without one enormous txn.
	tx := store.Begin()
	for i := 0; i < snapshotApplyBenchNodes; i++ {
		if err := tx.AddNode("n" + strconv.Itoa(i)); err != nil {
			tb.Fatalf("AddNode: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatalf("Commit nodes: %v", err)
	}

	const batch = 10_000
	for start := 0; start < snapshotApplyBenchEdges; start += batch {
		tx := store.Begin()
		for k := 0; k < batch && start+k < snapshotApplyBenchEdges; k++ {
			i := start + k
			src := "n" + strconv.Itoa(i%snapshotApplyBenchNodes)
			dst := "n" + strconv.Itoa((i*7919+13)%snapshotApplyBenchNodes)
			if err := tx.AddEdge(src, dst, int64(i)); err != nil {
				tb.Fatalf("AddEdge: %v", err)
			}
		}
		if err := tx.Commit(); err != nil {
			tb.Fatalf("Commit edges: %v", err)
		}
	}

	if checkpointed {
		var mu sync.Mutex
		cp := checkpoint.New[string, int64](checkpoint.Config{Dir: dir}, g, w, &mu)
		ctx, cancel := context.WithCancel(context.Background())
		cp.Start(ctx)
		if err := cp.Trigger(); err != nil {
			tb.Fatalf("checkpoint Trigger: %v", err)
		}
		cp.Stop()
		cancel()
	}
	if err := w.Close(); err != nil {
		tb.Fatalf("wal.Close: %v", err)
	}
	return dir
}

// recoverOnce opens dir and returns the recovery result, failing the benchmark
// on error or on an unexpected snapshot outcome.
func recoverOnce(tb testing.TB, dir string, wantSnapshot bool) Result[string, int64] {
	tb.Helper()
	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		tb.Fatalf("recovery.Open: %v", err)
	}
	if res.SnapshotHit != wantSnapshot {
		tb.Fatalf("SnapshotHit = %v, want %v", res.SnapshotHit, wantSnapshot)
	}
	return res
}

// BenchmarkSnapshotApply_Snapshot measures recovery from a checkpointed
// directory: the snapshot is self-sufficient and the WAL was truncated, so this
// is the snapshot-apply path in isolation.
func BenchmarkSnapshotApply_Snapshot(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping the 500k-edge recovery benchmark under -short")
	}
	// The fixture is read-only for recovery, so one build serves every
	// iteration; recovery never mutates the directory.
	dir := buildRecoveryFixture(b, true)
	res := recoverOnce(b, dir, true)
	if res.WALOps != 0 {
		b.Fatalf("WALOps = %d, want 0 — the snapshot must be self-sufficient", res.WALOps)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		recoverOnce(b, dir, true)
	}
}

// BenchmarkSnapshotApply_WALReplay measures recovery of the same graph with no
// checkpoint: every frame is replayed. It is the baseline the snapshot path must
// beat for a checkpoint to be worth taking.
func BenchmarkSnapshotApply_WALReplay(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping the 500k-edge recovery benchmark under -short")
	}
	dir := buildRecoveryFixture(b, false)
	res := recoverOnce(b, dir, false)
	if res.WALOps == 0 {
		b.Fatal("WALOps = 0, want the whole WAL replayed")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		recoverOnce(b, dir, false)
	}
}
