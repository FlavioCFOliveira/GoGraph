package recovery

import (
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// writeCodecOnlyMultigraphCrashState builds the post-crash on-disk state of
// a codec-only multigraph store interrupted at the
// checkpoint.post-snapshot-pre-truncate crash point: dir/snapshot already
// contains the committed A→B edge AND dir/wal still contains the frames
// that produced it (the truncate never ran). Recovery must apply the
// snapshot and then replay the WAL without doubling the edge.
func writeCodecOnlyMultigraphCrashState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	wlog, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	st := txn.NewStoreWithCodec(g, wlog, txn.NewStringCodec())

	tx := st.Begin()
	if err := tx.AddNode("A"); err != nil {
		t.Fatalf("AddNode A: %v", err)
	}
	if err := tx.AddNode("B"); err != nil {
		t.Fatalf("AddNode B: %v", err)
	}
	if err := tx.AddEdge("A", "B", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := g.AdjList().Size(); got != 1 {
		t.Fatalf("pre-snapshot edge count = %d, want 1", got)
	}

	// Checkpoint step 1: write a self-sufficient snapshot that already
	// contains the edge.
	cs := csr.BuildFromAdjList(g.AdjList())
	if err := snapshot.WriteSnapshotFull(filepath.Join(dir, "snapshot"), cs, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}

	// Simulated crash before checkpoint step 2: the WAL is NOT truncated,
	// so the AddEdge frames survive alongside the snapshot.
	if err := wlog.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
	return dir
}

// assertSingleABEdge fails the test unless the recovered graph holds
// exactly one A→B edge: present (not lost) and not doubled by the WAL
// replay over the snapshot.
func assertSingleABEdge(t *testing.T, res Result[string, float64]) {
	t.Helper()
	if !res.Graph.AdjList().HasEdge("A", "B") {
		t.Fatal("recovered graph lost the A->B edge")
	}
	if got := res.Graph.AdjList().Size(); got != 1 {
		t.Fatalf("recovered edge count = %d, want 1 (snapshot + WAL replay must not duplicate the edge)", got)
	}
}

// TestRecovery_CodecOnlyMultigraph_OpAddEdgeHIsIdempotent is the gate for
// the codec-only multigraph duplication bug: a store built with
// [txn.NewStoreWithCodec] (no weight codec) used to emit handle-less
// [txn.OpAddEdge] frames, which recovery replays via unconditional
// AddEdge — on a multigraph that appends a parallel edge even when the
// snapshot already restored it, doubling every edge after a crash at
// checkpoint.post-snapshot-pre-truncate. The codec-only path now emits
// handle-bearing [txn.OpAddEdgeH] frames, replayed idempotently via
// AddEdgeHIfAbsent. Recovery mirrors the producer configuration: a nil
// [Options.WeightCodec] selects the codec-only frame layout.
func TestRecovery_CodecOnlyMultigraph_OpAddEdgeHIsIdempotent(t *testing.T) {
	t.Parallel()
	dir := writeCodecOnlyMultigraphCrashState(t)

	res, err := Open[string, float64](dir, Options[string, float64]{
		Codec: txn.NewStringCodec(),
		// WeightCodec deliberately nil: mirrors txn.NewStoreWithCodec.
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false, want true (snapshot must have been applied)")
	}
	assertSingleABEdge(t, res)
}

// TestRecovery_CodecOnlyMultigraph_NonNilWeightCodecFallback pins the
// compatibility contract for pre-existing callers: before nil
// [Options.WeightCodec] was accepted, every consumer recovering a
// codec-only store was forced to pass some weight codec. Such a caller
// must still recover the weight-less [txn.OpAddEdgeH] frames correctly —
// the decoder detects that the weighted layout does not parse and falls
// back to the codec-only layout — and the multigraph dedup guarantee
// must hold identically.
func TestRecovery_CodecOnlyMultigraph_NonNilWeightCodecFallback(t *testing.T) {
	t.Parallel()
	dir := writeCodecOnlyMultigraphCrashState(t)

	res, err := Open[string, float64](dir, Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	assertSingleABEdge(t, res)
}
