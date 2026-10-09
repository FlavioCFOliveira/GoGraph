package recovery

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestRecovery_NilWeightCodecDiscardsWeightedTxn_2808 pins what recovery does
// with a [txn.OpAddEdgeWeighted] frame when [Options.WeightCodec] is nil, so the
// godoc of applyOpCodec and [Result] cannot drift back to promising a
// zero-weight fallback (rmp #2808).
//
// The WAL holds two committed transactions, each adding one weighted edge
// (a->b, then b->c), with no snapshot. Recovered WITH the weight codec, both
// replay. Recovered WITHOUT it, the first weighted op fails to apply: replay
// stops at that transaction, raises [ErrCommittedTxnCorruptOp], and discards it
// and every transaction after it, so nothing replays and [Open] still returns a
// nil error.
func TestRecovery_NilWeightCodecDiscardsWeightedTxn_2808(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{})
	s := txn.NewStoreWithOptions[string, int64](g, w, opts)
	for _, e := range [][2]string{{"a", "b"}, {"b", "c"}} {
		tx := s.Begin()
		if err := tx.AddEdge(e[0], e[1], 7); err != nil {
			t.Fatalf("AddEdge(%s,%s): %v", e[0], e[1], err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit(%s,%s): %v", e[0], e[1], err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	t.Run("with_weight_codec", func(t *testing.T) {
		t.Parallel()
		res, err := Open[string, int64](dir, OptionsFromTxn(opts))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if res.WALOps != 2 {
			t.Fatalf("WALOps = %d, want 2", res.WALOps)
		}
		if !res.IsClean() {
			t.Fatalf("IsClean() = false, TailErr = %v; want a clean recovery", res.TailErr)
		}
		adj := res.Graph.AdjList()
		if !adj.HasEdge("a", "b") || !adj.HasEdge("b", "c") {
			t.Fatal("a weighted edge did not replay with the weight codec present")
		}
		if got := readEdgeWeightInt64(t, res.Graph, "a", "b"); got != 7 {
			t.Fatalf("weight a->b = %d, want 7", got)
		}
	})

	t.Run("nil_weight_codec", func(t *testing.T) {
		t.Parallel()
		res, err := Open[string, int64](dir, Options[string, int64]{Codec: txn.NewStringCodec()})
		if err != nil {
			t.Fatalf("Open returned %v; a corrupt op inside a committed transaction must not fail the open", err)
		}
		if res.WALOps != 0 {
			t.Fatalf("WALOps = %d, want 0: the first weighted op stops replay", res.WALOps)
		}
		if !errors.Is(res.TailErr, ErrCommittedTxnCorruptOp) {
			t.Fatalf("TailErr = %v; want errors.Is(..., ErrCommittedTxnCorruptOp)", res.TailErr)
		}
		if res.IsClean() {
			t.Fatal("IsClean() = true; two acknowledged transactions were discarded")
		}
		mapper := res.Graph.AdjList().Mapper()
		for _, n := range []string{"a", "b", "c"} {
			if _, ok := mapper.Lookup(n); ok {
				t.Fatalf("node %q present; no transaction may replay once the first one is discarded", n)
			}
		}
		adj := res.Graph.AdjList()
		if adj.HasEdge("a", "b") || adj.HasEdge("b", "c") {
			t.Fatal("an edge replayed; there is no zero-weight fallback")
		}
	})
}
