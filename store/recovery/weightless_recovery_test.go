package recovery

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestCheckpointRecovery_WeightlessSurvives is the #1650 manifest-persistence
// gate. A weightless graph (adjlist.Config.Weightless), checkpointed and
// recovered, must come back WEIGHTLESS — the manifest persists the Weightless
// flag (omitempty, backward-compatible) so recovery rebuilds the graph
// weightless rather than silently re-allocating a zero-filled weight column,
// preserving the per-edge memory saving across a restart. Topology survives
// unchanged.
func TestCheckpointRecovery_WeightlessSurvives(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}

	g := lpg.New[string, int64](adjlist.Config{Directed: true, Weightless: true})
	if !g.AdjList().Weightless() {
		t.Fatal("precondition: source graph is not weightless")
	}
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	store := txn.NewStoreWithOptions[string, int64](g, w, opts)

	tx := store.Begin()
	mustTx(t, tx.AddNode("a"))
	mustTx(t, tx.AddNode("b"))
	mustTx(t, tx.AddNode("c"))
	// Weights are accepted but ignored under Weightless; they must read back as
	// the zero W and recover identically.
	mustTx(t, tx.AddEdge("a", "b", 99))
	mustTx(t, tx.AddEdge("b", "c", 7))
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var mu sync.Mutex
	cp := checkpoint.New[string, int64](checkpoint.Config{Dir: dir}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	cp.Stop()
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	rg := res.Graph

	// The recovered graph STAYS weightless — the whole point of #1650's manifest
	// field.
	if !rg.AdjList().Weightless() {
		t.Error("recovered graph is not weightless: the manifest did not persist/restore adjlist.Config.Weightless (#1650)")
	}
	// Topology survives.
	if !rg.AdjList().HasEdge("a", "b") || !rg.AdjList().HasEdge("b", "c") {
		t.Error("edges missing after weightless checkpoint+recovery")
	}
	// Weights read back as the zero value (weightless contract).
	for v, w := range rg.AdjList().Neighbours("a") {
		if v == "b" && w != 0 {
			t.Errorf("weightless edge a->b weight = %d, want 0", w)
		}
	}
}
