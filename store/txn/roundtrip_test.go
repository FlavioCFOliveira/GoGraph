package txn_test

import (
	"bytes"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// This file lives in the external txn_test package so it can import
// the recovery package without forming an import cycle (recovery
// itself imports txn).
//
// The tests below close the loop the production system actually walks:
// a transaction commits a mutation, the store is closed, recovery
// reopens the on-disk directory, and the recovered graph must reflect
// every previously committed op. Each named subtest exercises one
// mutation kind end-to-end across the typed codec path via
// NewStoreWithOptions.

// recoveryOpen reopens dir through recovery.Open with the canonical
// string+int64 codecs. The returned graph is the post-replay state.
func recoveryOpen(t *testing.T, dir string) *lpg.Graph[string, int64] {
	t.Helper()
	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	return res.Graph
}

// writeThenRecover runs setup on a fresh typed store backed by a
// per-test WAL, commits, closes the WAL, then reopens the directory
// via recovery and hands the recovered graph to assert.
func writeThenRecover(t *testing.T, setup func(*testing.T, *txn.Store[string, int64]), assert func(*testing.T, *lpg.Graph[string, int64])) {
	t.Helper()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := txn.NewStoreWithOptions[string, int64](g, w, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	setup(t, s)
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
	rec := recoveryOpen(t, dir)
	assert(t, rec)
}

// TestRoundtrip_Mutations_RecoverFromWAL walks every mutation kind in
// a single table. Every subtest commits a mutation, closes the WAL,
// reopens via recovery, and asserts the recovered graph matches what
// the in-memory apply would have produced.
//
//nolint:gocyclo // table-driven: one subtest per mutation kind
func TestRoundtrip_Mutations_RecoverFromWAL(t *testing.T) {
	t.Parallel()

	t.Run("AddNode_idempotent", func(t *testing.T) {
		t.Parallel()
		writeThenRecover(t,
			func(t *testing.T, s *txn.Store[string, int64]) {
				tx := s.Begin()
				if err := tx.AddNode("alice"); err != nil {
					t.Fatal(err)
				}
				if err := tx.AddNode("alice"); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			},
			func(t *testing.T, g *lpg.Graph[string, int64]) {
				if got := g.AdjList().Mapper().Len(); got != 1 {
					t.Fatalf("recovered mapper Len = %d, want 1", got)
				}
			},
		)
	})

	t.Run("SetNodeLabel_multiple_labels", func(t *testing.T) {
		t.Parallel()
		writeThenRecover(t,
			func(t *testing.T, s *txn.Store[string, int64]) {
				tx := s.Begin()
				for _, lbl := range []string{"Person", "Admin", "User"} {
					if err := tx.SetNodeLabel("alice", lbl); err != nil {
						t.Fatal(err)
					}
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			},
			func(t *testing.T, g *lpg.Graph[string, int64]) {
				for _, lbl := range []string{"Person", "Admin", "User"} {
					if !g.HasNodeLabel("alice", lbl) {
						t.Fatalf("recovered: label %q missing", lbl)
					}
				}
			},
		)
	})

	t.Run("RemoveNodeLabel", func(t *testing.T) {
		t.Parallel()
		writeThenRecover(t,
			func(t *testing.T, s *txn.Store[string, int64]) {
				tx := s.Begin()
				_ = tx.SetNodeLabel("alice", "Person")
				_ = tx.SetNodeLabel("alice", "Admin")
				_ = tx.RemoveNodeLabel("alice", "Admin")
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			},
			func(t *testing.T, g *lpg.Graph[string, int64]) {
				if !g.HasNodeLabel("alice", "Person") {
					t.Fatal("Person label missing after recovery")
				}
				if g.HasNodeLabel("alice", "Admin") {
					t.Fatal("Admin label resurrected by recovery")
				}
			},
		)
	})

	t.Run("RemoveNode_strips_labels_and_properties", func(t *testing.T) {
		t.Parallel()
		writeThenRecover(t,
			func(t *testing.T, s *txn.Store[string, int64]) {
				tx := s.Begin()
				_ = tx.AddNode("alice")
				_ = tx.SetNodeLabel("alice", "Person")
				_ = tx.SetNodeLabel("alice", "Admin")
				_ = tx.SetNodeProperty("alice", "name", lpg.StringValue("Alice"))
				_ = tx.SetNodeProperty("alice", "age", lpg.Int64Value(30))
				_ = tx.RemoveNode("alice")
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			},
			func(t *testing.T, g *lpg.Graph[string, int64]) {
				if g.HasNodeLabel("alice", "Person") {
					t.Fatal("Person label survived RemoveNode replay")
				}
				if g.HasNodeLabel("alice", "Admin") {
					t.Fatal("Admin label survived RemoveNode replay")
				}
				if _, ok := g.GetNodeProperty("alice", "name"); ok {
					t.Fatal("name property survived RemoveNode replay")
				}
				if _, ok := g.GetNodeProperty("alice", "age"); ok {
					t.Fatal("age property survived RemoveNode replay")
				}
				// Mapper entry is permanent in production semantics.
				if got := g.AdjList().Mapper().Len(); got != 1 {
					t.Fatalf("recovered mapper Len = %d, want 1", got)
				}
			},
		)
	})

	t.Run("SetNodeProperty_every_kind", func(t *testing.T) {
		t.Parallel()
		knownTime := time.Date(2026, 5, 22, 13, 0, 0, 0, time.UTC)
		writeThenRecover(t,
			func(t *testing.T, s *txn.Store[string, int64]) {
				tx := s.Begin()
				_ = tx.SetNodeProperty("alice", "name", lpg.StringValue("Alice"))
				_ = tx.SetNodeProperty("alice", "age", lpg.Int64Value(30))
				_ = tx.SetNodeProperty("alice", "score", lpg.Float64Value(99.5))
				_ = tx.SetNodeProperty("alice", "active", lpg.BoolValue(true))
				_ = tx.SetNodeProperty("alice", "joined", lpg.TimeValue(knownTime))
				_ = tx.SetNodeProperty("alice", "blob", lpg.BytesValue([]byte{1, 2, 3}))
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			},
			func(t *testing.T, g *lpg.Graph[string, int64]) {
				if v, ok := g.GetNodeProperty("alice", "name"); !ok {
					t.Fatal("name missing")
				} else if s, _ := v.String(); s != "Alice" {
					t.Fatalf("name = %q, want Alice", s)
				}
				if v, ok := g.GetNodeProperty("alice", "age"); !ok {
					t.Fatal("age missing")
				} else if i, _ := v.Int64(); i != 30 {
					t.Fatalf("age = %d, want 30", i)
				}
				if v, ok := g.GetNodeProperty("alice", "score"); !ok {
					t.Fatal("score missing")
				} else if f, _ := v.Float64(); math.Float64bits(f) != math.Float64bits(99.5) {
					t.Fatalf("score = %g, want 99.5", f)
				}
				if v, ok := g.GetNodeProperty("alice", "active"); !ok {
					t.Fatal("active missing")
				} else if b, _ := v.Bool(); !b {
					t.Fatal("active = false, want true")
				}
				if v, ok := g.GetNodeProperty("alice", "joined"); !ok {
					t.Fatal("joined missing")
				} else if tm, _ := v.Time(); !tm.Equal(knownTime) {
					t.Fatalf("joined = %v, want %v", tm, knownTime)
				}
				if v, ok := g.GetNodeProperty("alice", "blob"); !ok {
					t.Fatal("blob missing")
				} else if b, _ := v.Bytes(); !bytes.Equal(b, []byte{1, 2, 3}) {
					t.Fatalf("blob = %x, want 010203", b)
				}
			},
		)
	})

	t.Run("DelNodeProperty", func(t *testing.T) {
		t.Parallel()
		writeThenRecover(t,
			func(t *testing.T, s *txn.Store[string, int64]) {
				tx := s.Begin()
				_ = tx.SetNodeProperty("alice", "name", lpg.StringValue("Alice"))
				_ = tx.SetNodeProperty("alice", "age", lpg.Int64Value(30))
				_ = tx.DelNodeProperty("alice", "age")
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			},
			func(t *testing.T, g *lpg.Graph[string, int64]) {
				if _, ok := g.GetNodeProperty("alice", "age"); ok {
					t.Fatal("age survived DelNodeProperty replay")
				}
				if _, ok := g.GetNodeProperty("alice", "name"); !ok {
					t.Fatal("name dropped on replay")
				}
			},
		)
	})

	t.Run("SetEdgeProperty_every_kind", func(t *testing.T) {
		t.Parallel()
		knownTime := time.Date(2026, 5, 22, 14, 0, 0, 0, time.UTC)
		writeThenRecover(t,
			func(t *testing.T, s *txn.Store[string, int64]) {
				tx := s.Begin()
				_ = tx.AddEdge("alice", "bob", 0)
				_ = tx.SetEdgeProperty("alice", "bob", "since", lpg.StringValue("2026"))
				_ = tx.SetEdgeProperty("alice", "bob", "weight", lpg.Int64Value(7))
				_ = tx.SetEdgeProperty("alice", "bob", "score", lpg.Float64Value(0.81))
				_ = tx.SetEdgeProperty("alice", "bob", "verified", lpg.BoolValue(true))
				_ = tx.SetEdgeProperty("alice", "bob", "started", lpg.TimeValue(knownTime))
				_ = tx.SetEdgeProperty("alice", "bob", "raw", lpg.BytesValue([]byte{0xAA}))
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			},
			func(t *testing.T, g *lpg.Graph[string, int64]) {
				for _, k := range []string{"since", "weight", "score", "verified", "started", "raw"} {
					if _, ok := g.GetEdgeProperty("alice", "bob", k); !ok {
						t.Fatalf("edge property %q missing after replay", k)
					}
				}
				if v, ok := g.GetEdgeProperty("alice", "bob", "started"); ok {
					tm, _ := v.Time()
					if !tm.Equal(knownTime) {
						t.Fatalf("started = %v, want %v", tm, knownTime)
					}
				}
			},
		)
	})

	t.Run("DelEdgeProperty", func(t *testing.T) {
		t.Parallel()
		writeThenRecover(t,
			func(t *testing.T, s *txn.Store[string, int64]) {
				tx := s.Begin()
				_ = tx.AddEdge("alice", "bob", 0)
				_ = tx.SetEdgeProperty("alice", "bob", "since", lpg.StringValue("2026"))
				_ = tx.DelEdgeProperty("alice", "bob", "since")
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			},
			func(t *testing.T, g *lpg.Graph[string, int64]) {
				if _, ok := g.GetEdgeProperty("alice", "bob", "since"); ok {
					t.Fatal("since survived DelEdgeProperty replay")
				}
				if !g.AdjList().HasEdge("alice", "bob") {
					t.Fatal("edge missing after DelEdgeProperty replay")
				}
			},
		)
	})

	t.Run("RemoveEdge", func(t *testing.T) {
		t.Parallel()
		writeThenRecover(t,
			func(t *testing.T, s *txn.Store[string, int64]) {
				tx := s.Begin()
				_ = tx.AddEdge("alice", "bob", 0)
				_ = tx.AddEdge("alice", "carol", 0)
				_ = tx.RemoveEdge("alice", "bob")
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			},
			func(t *testing.T, g *lpg.Graph[string, int64]) {
				if g.AdjList().HasEdge("alice", "bob") {
					t.Fatal("alice->bob edge survived RemoveEdge replay")
				}
				if !g.AdjList().HasEdge("alice", "carol") {
					t.Fatal("alice->carol edge dropped on replay")
				}
			},
		)
	})

	t.Run("SetEdgeLabel", func(t *testing.T) {
		t.Parallel()
		writeThenRecover(t,
			func(t *testing.T, s *txn.Store[string, int64]) {
				tx := s.Begin()
				_ = tx.AddEdge("alice", "bob", 0)
				_ = tx.SetEdgeLabel("alice", "bob", "KNOWS")
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			},
			func(t *testing.T, g *lpg.Graph[string, int64]) {
				if !g.AdjList().HasEdge("alice", "bob") {
					t.Fatal("edge missing")
				}
				if !g.HasEdgeLabel("alice", "bob", "KNOWS") {
					t.Fatal("edge label missing after replay")
				}
			},
		)
	})

	t.Run("AddEdgeWeighted", func(t *testing.T) {
		t.Parallel()
		const want int64 = 0x0BADC0DE
		writeThenRecover(t,
			func(t *testing.T, s *txn.Store[string, int64]) {
				tx := s.Begin()
				if err := tx.AddEdge("alice", "bob", want); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			},
			func(t *testing.T, g *lpg.Graph[string, int64]) {
				var got int64
				for n, w := range g.AdjList().Neighbours("alice") {
					if n == "bob" {
						got = w
						break
					}
				}
				if got != want {
					t.Fatalf("weight = %d, want %d", got, want)
				}
			},
		)
	})
}

// TestRoundtrip_CommitWALOnly_RecoversIntoGraph documents the
// production invariant of CommitWALOnly: the in-memory graph remains
// untouched, but the WAL has every op, so recovery rebuilds the full
// state. This is the "eager apply, lazy WAL" path's safety net.
func TestRoundtrip_CommitWALOnly_RecoversIntoGraph(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := txn.NewStoreWithOptions[string, int64](g, w, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})

	tx := s.Begin()
	_ = tx.AddNode("alice")
	_ = tx.SetNodeLabel("alice", "Person")
	_ = tx.SetNodeProperty("alice", "name", lpg.StringValue("Alice"))
	_ = tx.AddEdge("alice", "bob", 0)
	_ = tx.SetEdgeProperty("alice", "bob", "since", lpg.StringValue("2026"))
	if err := tx.CommitWALOnly(); err != nil {
		t.Fatalf("CommitWALOnly: %v", err)
	}

	// In-memory graph must NOT carry any of the ops.
	if g.HasNodeLabel("alice", "Person") {
		t.Fatal("CommitWALOnly leaked label into graph")
	}
	if _, ok := g.GetNodeProperty("alice", "name"); ok {
		t.Fatal("CommitWALOnly leaked node property")
	}
	if g.AdjList().HasEdge("alice", "bob") {
		t.Fatal("CommitWALOnly leaked edge")
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Recovery must rebuild the full state.
	rec := recoveryOpen(t, dir)
	if !rec.HasNodeLabel("alice", "Person") {
		t.Fatal("recovered: Person label missing")
	}
	if v, ok := rec.GetNodeProperty("alice", "name"); !ok {
		t.Fatal("recovered: name property missing")
	} else if s, _ := v.String(); s != "Alice" {
		t.Fatalf("recovered: name = %q, want Alice", s)
	}
	if !rec.AdjList().HasEdge("alice", "bob") {
		t.Fatal("recovered: alice->bob edge missing")
	}
	if _, ok := rec.GetEdgeProperty("alice", "bob", "since"); !ok {
		t.Fatal("recovered: edge property since missing")
	}
}

// TestRoundtrip_Commit_AppendFailure_DurabilityHolds proves that
// when WAL Append fails mid-Commit (here forced by closing the
// writer before the Commit call), no op reaches either the WAL on
// disk or the in-memory graph: the durability-first contract
// guarantees an atomic boundary at Commit. Recovery on the resulting
// directory yields an empty graph.
func TestRoundtrip_Commit_AppendFailure_DurabilityHolds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := txn.NewStoreWithOptions[string, int64](g, w, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})

	tx := s.Begin()
	_ = tx.SetNodeLabel("alice", "Person")
	_ = tx.AddEdge("alice", "bob", 0)
	// Force the WAL to reject all subsequent writes.
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); !errors.Is(err, wal.ErrWriterClosed) {
		t.Fatalf("Commit err = %v, want ErrWriterClosed", err)
	}

	// Recovery must see an empty graph: no op was durably written.
	rec := recoveryOpen(t, dir)
	if rec.HasNodeLabel("alice", "Person") {
		t.Fatal("recovered graph carries label written by a failed Commit")
	}
	if rec.AdjList().HasEdge("alice", "bob") {
		t.Fatal("recovered graph carries edge written by a failed Commit")
	}
}
