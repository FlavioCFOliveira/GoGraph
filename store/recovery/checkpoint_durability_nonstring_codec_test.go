package recovery

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/checkpoint"
	"gograph/store/txn"
	"gograph/store/wal"
)

// TestCheckpointDurability_NonStringKeysCodecTruncates is the AC#1
// regression test for audit gap F3: with a mapper codec wired into the
// checkpointer ([checkpoint.WithMapperCodec]), a non-string snapshot is
// self-sufficient, so the checkpointer TRUNCATES the WAL and recovery
// reconstructs the full state from the snapshot ALONE.
//
// This is the inverse of [TestCheckpointDurability_NonStringKeysNotLost]
// (which covers the no-codec fallback where the WAL is retained). Here
// the test commits int64-keyed edges + a label + a property, checkpoints
// WITH the codec, then asserts:
//
//   - the WAL WAS truncated (size 0): the F2 guard is satisfied because
//     the snapshot now carries mapper.bin for the int64 key type;
//   - recovery replays ZERO WAL ops (WALOps == 0) yet still reconstructs
//     every committed edge, label, and property from the snapshot.
func TestCheckpointDurability_NonStringKeysCodecTruncates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}

	g := lpg.New[int64, int64](adjlist.Config{Directed: true})
	opts := txn.Options[int64, int64]{
		Codec:       txn.NewInt64Codec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	store := txn.NewStoreWithOptions[int64, int64](g, w, opts)

	tx := store.Begin()
	mustTx(t, tx.AddEdge(1, 2, 100))
	mustTx(t, tx.AddEdge(2, 3, 200))
	mustTx(t, tx.SetNodeLabel(1, "Root"))
	mustTx(t, tx.SetNodeProperty(2, "weight", lpg.Int64Value(42)))
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Codec-aware checkpoint: the int64 mapper is persisted, so the
	// snapshot is self-sufficient and the WAL must be truncated.
	var mu sync.Mutex
	cp := checkpoint.New[int64, int64](
		checkpoint.Config{Dir: dir}, g, w, &mu,
		checkpoint.WithMapperCodec[int64, int64](store.Codec()),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	cp.Stop()

	if got := cp.Stats().WALTruncBytes; got == 0 {
		t.Fatal("WALTruncBytes = 0: the codec-aware checkpoint must truncate the WAL")
	}

	// The WAL must have been truncated to empty.
	info, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("WAL size = %d, want 0 (codec snapshot is self-sufficient, WAL truncated)", info.Size())
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Recover from the snapshot alone (the WAL is empty).
	res, err := Open[int64, int64](dir, Options[int64, int64]{
		Codec:       txn.NewInt64Codec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false, want true")
	}
	if res.SnapshotSchemaVersion != 3 {
		t.Errorf("SnapshotSchemaVersion = %d, want 3 (self-sufficient v3 snapshot)", res.SnapshotSchemaVersion)
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0 (state must come from the snapshot alone)", res.WALOps)
	}
	rg := res.Graph
	if !rg.AdjList().HasEdge(1, 2) {
		t.Error("edge 1->2 lost after codec checkpoint + recovery-from-snapshot")
	}
	if !rg.AdjList().HasEdge(2, 3) {
		t.Error("edge 2->3 lost after codec checkpoint + recovery-from-snapshot")
	}
	// Weights must survive via the CSR snapshot.
	assertEdgeWeightInt64(t, rg, 1, 2, 100)
	assertEdgeWeightInt64(t, rg, 2, 3, 200)
	if !rg.HasNodeLabel(1, "Root") {
		t.Error("node label Root lost after codec checkpoint + recovery-from-snapshot")
	}
	if v, ok := rg.GetNodeProperty(2, "weight"); !ok {
		t.Error("node property weight lost after codec checkpoint + recovery-from-snapshot")
	} else if got, _ := v.Int64(); got != 42 {
		t.Errorf("node property weight = %d, want 42", got)
	}
}

// TestCheckpointDurability_UUIDKeysCodecTruncates exercises the same F3
// path for a [16]byte (UUID) key type, the other non-string codec the
// task calls out. It confirms the truncation + recover-from-snapshot
// behaviour is not specific to integer keys.
func TestCheckpointDurability_UUIDKeysCodecTruncates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}

	g := lpg.New[[16]byte, int64](adjlist.Config{Directed: true})
	opts := txn.Options[[16]byte, int64]{
		Codec:       txn.NewUUIDCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	store := txn.NewStoreWithOptions[[16]byte, int64](g, w, opts)

	a := [16]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
		0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x00}
	b := [16]byte{0x00, 0xFF, 0xEE, 0xDD, 0xCC, 0xBB, 0xAA, 0x99,
		0x88, 0x77, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11}
	const wantWeight int64 = 0xDEADBEEFCAFE

	tx := store.Begin()
	mustTx(t, tx.AddEdge(a, b, wantWeight))
	mustTx(t, tx.SetNodeLabel(a, "UUID"))
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var mu sync.Mutex
	cp := checkpoint.New[[16]byte, int64](
		checkpoint.Config{Dir: dir}, g, w, &mu,
		checkpoint.WithMapperCodec[[16]byte, int64](store.Codec()),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	cp.Stop()

	info, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("WAL size = %d, want 0 (UUID snapshot is self-sufficient)", info.Size())
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	res, err := Open[[16]byte, int64](dir, Options[[16]byte, int64]{
		Codec:       txn.NewUUIDCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0 (state must come from the snapshot alone)", res.WALOps)
	}
	rg := res.Graph
	if !rg.AdjList().HasEdge(a, b) {
		t.Fatal("UUID edge lost after codec checkpoint + recovery-from-snapshot")
	}
	assertEdgeWeightInt64UUID(t, rg, a, b, wantWeight)
	if !rg.HasNodeLabel(a, "UUID") {
		t.Error("UUID node label lost after codec checkpoint + recovery-from-snapshot")
	}
}

// assertEdgeWeightInt64 fails the test unless g holds an edge src->dst
// with the given int64 weight.
func assertEdgeWeightInt64(t *testing.T, g *lpg.Graph[int64, int64], src, dst, want int64) {
	t.Helper()
	for n, wt := range g.AdjList().Neighbours(src) {
		if n == dst {
			if wt != want {
				t.Errorf("edge %d->%d weight = %d, want %d", src, dst, wt, want)
			}
			return
		}
	}
	t.Errorf("edge %d->%d not found", src, dst)
}

// assertEdgeWeightInt64UUID is the [16]byte-keyed counterpart of
// assertEdgeWeightInt64.
func assertEdgeWeightInt64UUID(t *testing.T, g *lpg.Graph[[16]byte, int64], src, dst [16]byte, want int64) {
	t.Helper()
	for n, wt := range g.AdjList().Neighbours(src) {
		if n == dst {
			if wt != want {
				t.Errorf("UUID edge weight = 0x%X, want 0x%X", wt, want)
			}
			return
		}
	}
	t.Error("UUID edge not found")
}
