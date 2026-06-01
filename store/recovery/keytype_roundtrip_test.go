package recovery

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestRecovery_KeyTypeRoundTrip exercises [Open] across two key-type
// instantiations that are NOT already covered by the exhaustive suite
// in open_typed_test.go:
//
//   - (string, float64): string node-IDs with floating-point weights.
//     The canonical NewFloat64WeightCodec must survive the snapshot
//     path (no WAL weight encoding today) without loss.
//   - ([16]byte, int64): UUID node-IDs with int64 weights, exercising
//     the fixed-width UUID codec in a snapshot context.
//
// Each sub-test follows the canonical pattern: commit a deterministic
// workload through a typed Store (v2 frames), take a v3 snapshot,
// close the WAL, and re-open via [Open]. The recovered graph is then
// inspected for edge presence, weight equality, label, and property
// survival.
func TestRecovery_KeyTypeRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("string_float64", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		w, err := wal.Open(filepath.Join(dir, "wal"))
		if err != nil {
			t.Fatalf("wal.Open: %v", err)
		}
		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		opts := txn.Options[string, float64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewFloat64WeightCodec(),
		}
		s := txn.NewStoreWithOptions[string, float64](g, w, opts)

		const wantWeight = math.E // transcendental: must not round-trip to zero
		type edge struct{ src, dst string }
		edges := []edge{{"p", "q"}, {"q", "r"}, {"r", "p"}}
		for _, e := range edges {
			tx := s.Begin()
			if err := tx.AddEdge(e.src, e.dst, wantWeight); err != nil {
				t.Fatalf("AddEdge(%s->%s): %v", e.src, e.dst, err)
			}
			if err := tx.SetNodeLabel(e.src, "Node"); err != nil {
				t.Fatalf("SetNodeLabel(%s): %v", e.src, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("Commit: %v", err)
			}
		}
		if err := g.SetNodeProperty("p", "x", lpg.Float64Value(wantWeight)); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}

		c := csr.BuildFromAdjList(g.AdjList())
		if err := snapshot.WriteSnapshotFull(filepath.Join(dir, "snapshot"), c, g); err != nil {
			t.Fatalf("WriteSnapshotFull: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("wal Close: %v", err)
		}

		res, err := Open[string, float64](dir, Options[string, float64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewFloat64WeightCodec(),
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if !res.SnapshotHit {
			t.Fatal("SnapshotHit = false, want true")
		}

		for _, e := range edges {
			if !res.Graph.AdjList().HasEdge(e.src, e.dst) {
				t.Errorf("edge %s->%s missing post-recovery", e.src, e.dst)
			}
			var got float64
			for n, w := range res.Graph.AdjList().Neighbours(e.src) {
				if n == e.dst {
					got = w
				}
			}
			if math.Float64bits(got) != math.Float64bits(wantWeight) {
				t.Errorf("edge %s->%s weight bits=0x%x, want 0x%x",
					e.src, e.dst, math.Float64bits(got), math.Float64bits(wantWeight))
			}
			if !res.Graph.HasNodeLabel(e.src, "Node") {
				t.Errorf("node %s missing label Node", e.src)
			}
		}

		// Check float64 property survived via snapshot properties.bin.
		if v, ok := res.Graph.GetNodeProperty("p", "x"); !ok {
			t.Error("node p missing property x")
		} else if f, _ := v.Float64(); math.Float64bits(f) != math.Float64bits(wantWeight) {
			t.Errorf("node p property x = %v, want %v", f, wantWeight)
		}
	})

	t.Run("uuid_int64", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		w, err := wal.Open(filepath.Join(dir, "wal"))
		if err != nil {
			t.Fatalf("wal.Open: %v", err)
		}
		g := lpg.New[[16]byte, int64](adjlist.Config{Directed: true})
		opts := txn.Options[[16]byte, int64]{
			Codec:       txn.NewUUIDCodec(),
			WeightCodec: txn.NewInt64WeightCodec(),
		}
		s := txn.NewStoreWithOptions[[16]byte, int64](g, w, opts)

		src := [16]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
			0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x00}
		dst := [16]byte{0x00, 0xFF, 0xEE, 0xDD, 0xCC, 0xBB, 0xAA, 0x99,
			0x88, 0x77, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11}
		const wantWeight int64 = 0xDEADBEEFCAFE

		tx := s.Begin()
		if err := tx.AddEdge(src, dst, wantWeight); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		if err := tx.SetNodeLabel(src, "UUID"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}

		// [16]byte mapper is not string-keyed; the snapshot will be v2
		// (no mapper.bin). WAL carries the op.
		if err := w.Close(); err != nil {
			t.Fatalf("wal Close: %v", err)
		}

		res, err := Open[[16]byte, int64](dir, Options[[16]byte, int64]{
			Codec:       txn.NewUUIDCodec(),
			WeightCodec: txn.NewInt64WeightCodec(),
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if res.WALOps != 2 { // AddEdge + SetNodeLabel
			t.Fatalf("WALOps = %d, want 2", res.WALOps)
		}
		if !res.Graph.AdjList().HasEdge(src, dst) {
			t.Fatal("UUID edge missing post-recovery")
		}
		var got int64
		for n, wt := range res.Graph.AdjList().Neighbours(src) {
			if n == dst {
				got = wt
			}
		}
		if got != wantWeight {
			t.Fatalf("UUID edge weight = 0x%X, want 0x%X", got, wantWeight)
		}
		if !res.Graph.HasNodeLabel(src, "UUID") {
			t.Fatal("UUID node missing label UUID")
		}
	})
}

// readEdgeWeightFloat64 is declared in weight_replay_test.go;
// we use it from there rather than redeclaring here.
