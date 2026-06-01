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

// TestRecovery_Open_CustomCodec exercises [Open] with an explicit
// [Options] that carries both a non-default Codec (string) and a
// non-default WeightCodec (float64). The test verifies that:
//
//  1. Both codec fields are honoured during WAL replay (the weight is
//     recovered correctly through the typed codec path).
//  2. The snapshot-resident properties survive independently of the
//     WAL replay (properties.bin path).
//  3. [Result.WALOps] matches the exact number of committed ops,
//     confirming the replay counted every frame.
//
// This is distinct from [TestTxn_RoundtripWeightedEdge_Recovery] which
// covers the weight round-trip in isolation. This test additionally
// verifies the snapshot + WAL integration path and checks the Options
// struct field pass-through.
func TestRecovery_Open_CustomCodec(t *testing.T) {
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

	// Commit edges with transcendental weights that must round-trip
	// bit-exactly through the float64 codec.
	type edge struct {
		src, dst string
		weight   float64
		label    string
	}
	edges := []edge{
		{"x", "y", math.Pi, "PI_EDGE"},
		{"y", "z", math.E, "E_EDGE"},
		{"z", "x", math.Phi, "PHI_EDGE"},
	}
	wantOps := 0
	for _, e := range edges {
		tx := s.Begin()
		if err := tx.AddEdge(e.src, e.dst, e.weight); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", e.src, e.dst, err)
		}
		wantOps++
		if err := tx.SetEdgeLabel(e.src, e.dst, e.label); err != nil {
			t.Fatalf("SetEdgeLabel: %v", err)
		}
		wantOps++
		if err := tx.SetNodeLabel(e.src, "Vertex"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		wantOps++
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}

	// Store a typed property in the snapshot (not in the WAL).
	if err := g.SetNodeProperty("x", "pi", lpg.Float64Value(math.Pi)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	c := csr.BuildFromAdjList(g.AdjList())
	if err := snapshot.WriteSnapshotFull(filepath.Join(dir, "snapshot"), c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	// Recover via Open, converting the txn.Options used by the writer
	// into the field-identical recovery.Options.
	res, err := Open[string, float64](dir, Options[string, float64](opts))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false, want true")
	}
	if res.WALOps != wantOps {
		t.Fatalf("WALOps = %d, want %d", res.WALOps, wantOps)
	}

	// Verify each edge with its exact weight and label.
	for _, e := range edges {
		if !res.Graph.AdjList().HasEdge(e.src, e.dst) {
			t.Errorf("edge %s->%s missing", e.src, e.dst)
			continue
		}
		var got float64
		for n, w := range res.Graph.AdjList().Neighbours(e.src) {
			if n == e.dst {
				got = w
			}
		}
		if math.Float64bits(got) != math.Float64bits(e.weight) {
			t.Errorf("edge %s->%s weight bits=0x%x, want 0x%x",
				e.src, e.dst, math.Float64bits(got), math.Float64bits(e.weight))
		}
		if !res.Graph.HasEdgeLabel(e.src, e.dst, e.label) {
			t.Errorf("edge %s->%s missing label %q", e.src, e.dst, e.label)
		}
		if !res.Graph.HasNodeLabel(e.src, "Vertex") {
			t.Errorf("node %s missing label Vertex", e.src)
		}
	}

	// Verify the property stored exclusively in the snapshot survives.
	if v, ok := res.Graph.GetNodeProperty("x", "pi"); !ok {
		t.Error("node x missing property pi")
	} else if f, _ := v.Float64(); math.Float64bits(f) != math.Float64bits(math.Pi) {
		t.Errorf("node x pi = %v, want %v", f, math.Pi)
	}
}
