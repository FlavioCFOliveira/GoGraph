package txn

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/wal"
)

// openWeightedStore builds an int64-keyed string-node store wired
// with the canonical StringCodec and Int64WeightCodec. The helper
// mirrors openStore in txn_test.go but takes the weight codec as a
// parameter so the same scaffolding can drive the int64 and float64
// variants of TestTxn_RoundtripWeightedEdge.
func openWeightedStoreInt64(t *testing.T) (store *Store[string, int64], walPath string, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	w, err := wal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store = NewStoreWithOptions[string, int64](g, w, Options[string, int64]{
		Codec:       NewStringCodec(),
		WeightCodec: NewInt64WeightCodec(),
	})
	walPath = path
	cleanup = func() {
		_ = w.Close()
		_ = os.RemoveAll(dir)
	}
	return store, walPath, cleanup
}

func openWeightedStoreFloat64(t *testing.T) (store *Store[string, float64], walPath string, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	w, err := wal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store = NewStoreWithOptions[string, float64](g, w, Options[string, float64]{
		Codec:       NewStringCodec(),
		WeightCodec: NewFloat64WeightCodec(),
	})
	walPath = path
	cleanup = func() {
		_ = w.Close()
		_ = os.RemoveAll(dir)
	}
	return store, walPath, cleanup
}

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

// TestTxn_RoundtripWeightedEdge is the acceptance test recorded on
// rmp #174: commit a weighted edge through the Tx surface, drop and
// re-open the store via the recovery package, and assert that the
// weight survives end-to-end. The matrix covers int64 (varint-shaped)
// and float64 (fixed-8-byte-shaped) weights — the two canonical
// built-in [WeightCodec] instantiations.
func TestTxn_RoundtripWeightedEdge(t *testing.T) {
	t.Parallel()
	t.Run("int64", func(t *testing.T) {
		t.Parallel()
		s, _, cleanup := openWeightedStoreInt64(t)
		defer cleanup()
		const want int64 = 0x7FFF_FFFF_DEAD_BEEF
		tx := s.Begin()
		if err := tx.AddEdge("alice", "bob", want); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		got := readEdgeWeightInt64(t, s.Graph(), "alice", "bob")
		if got != want {
			t.Fatalf("in-memory weight mismatch: got %d want %d", got, want)
		}
	})
	t.Run("float64", func(t *testing.T) {
		t.Parallel()
		s, _, cleanup := openWeightedStoreFloat64(t)
		defer cleanup()
		const want = 1.7320508075688772 // sqrt(3)
		tx := s.Begin()
		if err := tx.AddEdge("alice", "bob", want); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		got := readEdgeWeightFloat64(t, s.Graph(), "alice", "bob")
		if math.Float64bits(got) != math.Float64bits(want) {
			t.Fatalf("in-memory weight mismatch: got %v want %v", got, want)
		}
	})
}

// TestTxn_AddEdge_NoWeightCodec_ZeroOK confirms that stores
// constructed without a [WeightCodec] still accept zero-valued
// AddEdge calls and buffer them as an [OpAddEdge] record. This is
// the path that legacy callers continue to use after the signature
// change.
func TestTxn_AddEdge_NoWeightCodec_ZeroOK(t *testing.T) {
	t.Parallel()
	t.Run("legacy fmt codec", func(t *testing.T) {
		t.Parallel()
		s, _, cleanup := openStore(t)
		defer cleanup()
		tx := s.Begin()
		if err := tx.AddEdge("alice", "bob", 0); err != nil {
			t.Fatalf("AddEdge zero: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if !s.Graph().AdjList().HasEdge("alice", "bob") {
			t.Fatal("zero-weight AddEdge did not apply")
		}
	})
	t.Run("typed codec without weight codec", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "wal")
		w, err := wal.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = w.Close() }()
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		s := NewStoreWithCodec[string, int64](g, w, NewStringCodec())
		tx := s.Begin()
		if err := tx.AddEdge("alice", "bob", 0); err != nil {
			t.Fatalf("AddEdge zero: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if !s.Graph().AdjList().HasEdge("alice", "bob") {
			t.Fatal("zero-weight AddEdge did not apply")
		}
	})
}

// TestTxn_AddEdge_NoWeightCodec_NonzeroErr confirms that a store
// without a [WeightCodec] refuses to buffer a non-zero weight; the
// transaction surfaces [ErrNoWeightCodec] and no op is committed.
func TestTxn_AddEdge_NoWeightCodec_NonzeroErr(t *testing.T) {
	t.Parallel()
	t.Run("legacy fmt codec", func(t *testing.T) {
		t.Parallel()
		s, _, cleanup := openStore(t)
		defer cleanup()
		tx := s.Begin()
		if err := tx.AddEdge("alice", "bob", int64(7)); !errors.Is(err, ErrNoWeightCodec) {
			t.Fatalf("AddEdge(7) err = %v, want ErrNoWeightCodec", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if s.Graph().AdjList().HasEdge("alice", "bob") {
			t.Fatal("refused AddEdge should not appear in graph")
		}
	})
	t.Run("typed codec without weight codec", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "wal")
		w, err := wal.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = w.Close() }()
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		s := NewStoreWithCodec[string, int64](g, w, NewStringCodec())
		tx := s.Begin()
		if err := tx.AddEdge("alice", "bob", int64(42)); !errors.Is(err, ErrNoWeightCodec) {
			t.Fatalf("AddEdge(42) err = %v, want ErrNoWeightCodec", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	})
}

// TestTxn_Options_NewStoreWithOptions_AccessorsAndV2Frame confirms
// that NewStoreWithOptions records both codecs on the Store and that
// a weighted commit produces a v2-tagged frame whose kind byte is
// OpAddEdgeWeighted.
func TestTxn_Options_NewStoreWithOptions_AccessorsAndV2Frame(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	w, err := wal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := NewStoreWithOptions[string, int64](g, w, Options[string, int64]{
		Codec:       NewStringCodec(),
		WeightCodec: NewInt64WeightCodec(),
	})
	if s.Codec() == nil || s.WeightCodec() == nil {
		t.Fatal("Options-built store missing one of its codecs")
	}
	tx := s.Begin()
	if err := tx.AddEdge("alice", "bob", int64(99)); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := wal.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	var seen int
	if err := r.Replay(func(f wal.Frame) error {
		seen++
		if len(f.Payload) < 2 {
			t.Fatalf("payload too short: %d", len(f.Payload))
		}
		if f.Payload[0] != OpRecordV2 {
			t.Fatalf("first byte = 0x%02x, want OpRecordV2 = 0x%02x", f.Payload[0], OpRecordV2)
		}
		if f.Payload[1] != byte(OpAddEdgeWeighted) {
			t.Fatalf("kind byte = 0x%02x, want OpAddEdgeWeighted = 0x%02x", f.Payload[1], byte(OpAddEdgeWeighted))
		}
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if seen != 1 {
		t.Fatalf("frame count = %d, want 1", seen)
	}
}

// TestTxn_Options_ZeroWeightEmitsOpAddEdgeWeighted documents the
// invariant on the weighted store: once a [WeightCodec] is wired in,
// every AddEdge buffers an [OpAddEdgeWeighted] frame, including
// zero-valued weights. This keeps the wire-level layout unambiguous
// (every weighted store always writes the same shape).
//
// Forward-compatible replay of a pre-T8 WAL (where every AddEdge
// frame is [OpAddEdge]) is exercised separately in the recovery
// package: see TestTxn_ForwardCompat_PreT8WALReplays.
func TestTxn_Options_ZeroWeightEmitsOpAddEdgeWeighted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	w, err := wal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := NewStoreWithOptions[string, int64](g, w, Options[string, int64]{
		Codec:       NewStringCodec(),
		WeightCodec: NewInt64WeightCodec(),
	})
	tx := s.Begin()
	if err := tx.AddEdge("alice", "bob", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := wal.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if err := r.Replay(func(f wal.Frame) error {
		if f.Payload[1] != byte(OpAddEdgeWeighted) {
			t.Fatalf("weighted store commit emitted kind 0x%02x, want OpAddEdgeWeighted=0x%02x", f.Payload[1], byte(OpAddEdgeWeighted))
		}
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
}
