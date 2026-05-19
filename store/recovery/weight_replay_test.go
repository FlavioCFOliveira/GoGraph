package recovery

import (
	"math"
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/txn"
	"gograph/store/wal"
)

// readEdgeWeightInt64 returns the weight on the first edge from src
// to dst in g, or fails the test if no such edge exists.
func readEdgeWeightInt64(t *testing.T, g *lpg.Graph[string, int64], src, dst string) int64 {
	t.Helper()
	for n, w := range g.AdjList().Neighbours(src) {
		if n == dst {
			return w
		}
	}
	t.Fatalf("no edge %s -> %s in graph", src, dst)
	return 0
}

func readEdgeWeightFloat64(t *testing.T, g *lpg.Graph[string, float64], src, dst string) float64 {
	t.Helper()
	for n, w := range g.AdjList().Neighbours(src) {
		if n == dst {
			return w
		}
	}
	t.Fatalf("no edge %s -> %s in graph", src, dst)
	return 0
}

// TestTxn_RoundtripWeightedEdge_Recovery is the recovery-side
// counterpart of [TestTxn_RoundtripWeightedEdge] in store/txn: write
// a weighted edge through a [txn.NewStoreWithOptions] store, close
// the WAL, then reopen via [OpenWithOptions] and assert the weight
// survived. Covers the two canonical built-in [txn.WeightCodec]
// instantiations required by the rmp #174 acceptance criterion.
func TestTxn_RoundtripWeightedEdge_Recovery(t *testing.T) {
	t.Parallel()
	t.Run("int64", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		const want int64 = 0x0123_4567_89AB_CDEF
		// Write phase.
		w, err := wal.Open(filepath.Join(dir, "wal"))
		if err != nil {
			t.Fatal(err)
		}
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		opts := txn.Options[string, int64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewInt64WeightCodec(),
		}
		s := txn.NewStoreWithOptions[string, int64](g, w, opts)
		tx := s.Begin()
		if err := tx.AddEdge("alice", "bob", want); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		// Recover phase.
		res, err := OpenWithOptions[string, int64](dir, opts)
		if err != nil {
			t.Fatalf("OpenWithOptions: %v", err)
		}
		if res.WALOps != 1 {
			t.Fatalf("WALOps = %d, want 1", res.WALOps)
		}
		got := readEdgeWeightInt64(t, res.Graph, "alice", "bob")
		if got != want {
			t.Fatalf("recovered weight mismatch: got %d want %d", got, want)
		}
	})

	t.Run("float64", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		const want = 2.718281828459045 // e
		w, err := wal.Open(filepath.Join(dir, "wal"))
		if err != nil {
			t.Fatal(err)
		}
		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		opts := txn.Options[string, float64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewFloat64WeightCodec(),
		}
		s := txn.NewStoreWithOptions[string, float64](g, w, opts)
		tx := s.Begin()
		if err := tx.AddEdge("alice", "bob", want); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		res, err := OpenWithOptions[string, float64](dir, opts)
		if err != nil {
			t.Fatalf("OpenWithOptions: %v", err)
		}
		if res.WALOps != 1 {
			t.Fatalf("WALOps = %d, want 1", res.WALOps)
		}
		got := readEdgeWeightFloat64(t, res.Graph, "alice", "bob")
		if math.Float64bits(got) != math.Float64bits(want) {
			t.Fatalf("recovered weight mismatch: got %v want %v", got, want)
		}
	})
}

// TestTxn_ForwardCompat_PreT8WALReplays writes a pre-T8 WAL (only
// [txn.OpAddEdge] frames, no weight payload) under post-T8 code and
// then re-opens it with a [txn.WeightCodec] registered via
// [OpenWithOptions]. Forward compatibility means the apply path
// writes the zero value of W to the graph for the unweighted
// records, without erroring on the missing weight section.
func TestTxn_ForwardCompat_PreT8WALReplays(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Write a v2 WAL with only OpAddEdge frames (no weight section).
	// This mirrors what a post-#173 / pre-#174 store would have
	// emitted: typed N codec, no weight codec.
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	pre := txn.NewStoreWithCodec[string, int64](g, w, txn.NewStringCodec())
	tx := pre.Begin()
	if err := tx.AddEdge("alice", "bob", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := tx.SetEdgeLabel("alice", "bob", "KNOWS"); err != nil {
		t.Fatalf("SetEdgeLabel: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Re-open under post-T8 code with the typed weight codec wired
	// in. The unweighted commit must still apply, with W=zero.
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	res, err := OpenWithOptions[string, int64](dir, opts)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	if res.WALOps != 2 {
		t.Fatalf("WALOps = %d, want 2", res.WALOps)
	}
	if !res.Graph.AdjList().HasEdge("alice", "bob") {
		t.Fatal("pre-T8 AddEdge frame did not replay")
	}
	got := readEdgeWeightInt64(t, res.Graph, "alice", "bob")
	if got != 0 {
		t.Fatalf("pre-T8 frame must apply zero weight; got %d", got)
	}
	if !res.Graph.HasEdgeLabel("alice", "bob", "KNOWS") {
		t.Fatal("pre-T8 SetEdgeLabel frame did not replay")
	}
}

// TestOpenWithOptions_NilCodec rejects nil codecs.
func TestOpenWithOptions_NilCodec(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := OpenWithOptions[string, int64](dir, txn.Options[string, int64]{
		WeightCodec: txn.NewInt64WeightCodec(),
	}); err == nil {
		t.Fatal("OpenWithOptions with nil codec must error")
	}
	if _, err := OpenWithOptions[string, int64](dir, txn.Options[string, int64]{
		Codec: txn.NewStringCodec(),
	}); err == nil {
		t.Fatal("OpenWithOptions with nil weight codec must error")
	}
}

// TestOpenWithCodec_WeightedFrame_FallsBackToZero asserts that the
// codec-only open path (no weight codec) drops OpAddEdgeWeighted
// records rather than mis-parsing their tail. The graph stays empty
// of the edge and the fallback counter fires. Callers that need to
// preserve weights must use OpenWithOptions instead.
func TestOpenWithCodec_WeightedFrame_FallsBackToZero(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	s := txn.NewStoreWithOptions[string, int64](g, w, opts)
	tx := s.Begin()
	if err := tx.AddEdge("alice", "bob", int64(42)); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// Replay through the codec-only path; the frame must be dropped
	// (no edge in the graph) without crashing.
	res, err := OpenWithCodec[string, int64](dir, txn.NewStringCodec())
	if err != nil {
		t.Fatalf("OpenWithCodec: %v", err)
	}
	if res.Graph.AdjList().HasEdge("alice", "bob") {
		t.Fatal("OpenWithCodec without WeightCodec must drop OpAddEdgeWeighted frames")
	}
}

// TestOpenString_WeightedFrame_FallsBackToZero asserts the same
// behaviour on the string-keyed open path that has no WeightCodec
// at all: weighted frames are silently dropped (the fallback metric
// fires; no panic; no edge created).
func TestOpenString_WeightedFrame_FallsBackToZero(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	s := txn.NewStoreWithOptions[string, int64](g, w, opts)
	tx := s.Begin()
	if err := tx.AddEdge("alice", "bob", int64(42)); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := OpenString(dir)
	if err != nil {
		t.Fatalf("OpenString: %v", err)
	}
	if res.Graph.AdjList().HasEdge("alice", "bob") {
		t.Fatal("OpenString must drop OpAddEdgeWeighted frames")
	}
}
