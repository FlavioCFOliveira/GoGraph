package cypher_test

// rollback_undirected_edges_test.go — regression tests for rmp #2886: on an
// UNDIRECTED multigraph a rolled-back transaction that removes relationships
// must leave the committed graph byte-for-byte unchanged.
//
// An undirected relationship occupies two adjacency slots sharing one handle:
// the slot it was created as (a→b) and its mirror (b→a). Its type and
// properties are stored under the creation order only, and removing it clears
// both orders. The undo pre-image was captured in the order the removal was
// reached through, so a removal reached through the mirror — DETACH DELETE of
// the relationship's end node, or a DELETE of a relationship bound through its
// mirror slot — rolled back to a reversed relationship with no type and no
// properties.
//
// The engine warns that undirected storage does not give openCypher read
// semantics (cypher/api.go); these tests do not assert read semantics. They
// assert only that a rollback does not change what was committed, whatever the
// storage reads back.
//
// Layer: short. Engines, graphs and stores are local; the suite is goleak-clean.

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// undirectedConfig is the storage shape under test.
var undirectedConfig = adjlist.Config{}

// undirectedEdgeStatements are every #2885 statement plus the removals that
// reach a relationship through its mirror slot on an undirected graph: a
// directed pattern anchored on a relationship's end node binds the mirror, and
// node 3 is the end node of four committed relationships, three of them
// parallel. Node 4 carries the self-loop, which has no mirror.
var undirectedEdgeStatements = append(slices.Clone(parallelEdgeStatements),
	"MATCH (a:S {k: 1})-[r]->(b) DELETE r RETURN b.k",
	"MATCH (a:S {k: 3})-[r]->(b) DELETE r RETURN b.k",
	"MATCH (a:S {k: 3})-[r:U]-(b) DELETE r RETURN b.k",
	"MATCH (a:S {k: 3}) DETACH DELETE a",
	"MATCH (a:S {k: 4}) DETACH DELETE a",
	"MATCH (a:S {k: 3})-[r]->(b) SET r.z = 1 RETURN b.k",
)

// TestRollback_UndirectedEdges_InMemory_2886 covers the in-memory engine over an
// undirected multigraph. Each statement runs in its own subtest over a freshly
// seeded graph, and the final subtest replays the whole list over ONE graph,
// for the reason [TestRollback_ParallelEdges_InMemory_2885] gives.
func TestRollback_UndirectedEdges_InMemory_2886(t *testing.T) {
	newSeeded := func(t *testing.T) (*cypher.Engine, []string) {
		t.Helper()
		g := lpg.New[string, float64](undirectedConfig)
		eng := cypher.NewEngine(g)
		seedParallelEdges(t, eng)
		base := graphDump(t, eng)
		if len(base) == 0 {
			t.Fatal("seed produced an empty graph")
		}
		return eng, base
	}
	for _, stmt := range undirectedEdgeStatements {
		t.Run(stmt, func(t *testing.T) {
			eng, base := newSeeded(t)
			assertRollbackLeavesGraphUnchanged(t, eng, base, stmt)
		})
	}
	t.Run("sequence", func(t *testing.T) {
		eng, base := newSeeded(t)
		for _, stmt := range undirectedEdgeStatements {
			assertRollbackLeavesGraphUnchanged(t, eng, base, stmt)
		}
	})
}

// TestRollback_UndirectedEdges_Durable_2886 covers the WAL-backed engine over an
// undirected multigraph and what recovery returns afterwards. A fresh directory
// recovers as a DIRECTED graph, so the first open builds the undirected graph
// itself; the final checkpoint persists its shape in the snapshot manifest, and
// the reopen must return it. The statements are rolled back again after the
// reopen, over the relationships recovery rebuilt, and the graph is compared
// with the baseline once more.
func TestRollback_UndirectedEdges_Durable_2886(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](undirectedConfig)
	eng := cypher.NewEngineWithStore(txn.NewStoreWithOptions[string, float64](g, w, parallelEdgeStoreOpts()))
	seedParallelEdges(t, eng)
	base := graphDump(t, eng)
	if len(base) == 0 {
		t.Fatal("seed produced an empty graph")
	}
	for _, stmt := range undirectedEdgeStatements {
		assertRollbackLeavesGraphUnchanged(t, eng, base, stmt)
	}

	var mu sync.Mutex
	cp := checkpoint.New[string, float64](checkpoint.Config{Dir: dir}, g, w, &mu)
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(cctx)
	db := store.New(w, store.WithCheckpointer(cp), store.WithFinalCheckpoint())
	if err := db.Close(); err != nil {
		t.Fatalf("store.DB.Close: %v", err)
	}
	if st := cp.Stats(); st.LastError != "" {
		t.Fatalf("final checkpoint failed: %v", st.LastError)
	}

	res, err := recovery.Open[string, float64](dir, parallelEdgeRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	w2, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open (reopen): %v", err)
	}
	defer func() {
		if err := w2.Close(); err != nil {
			t.Errorf("wal.Close: %v", err)
		}
	}()
	eng2 := cypher.NewEngineWithStore(txn.NewStoreWithOptions[string, float64](res.Graph, w2, parallelEdgeStoreOpts()))
	if got := graphDump(t, eng2); !slices.Equal(got, base) {
		t.Fatalf("after checkpoint + reopen the recovered graph differs from the committed one:\n got  %v\n want %v", got, base)
	}
	for _, stmt := range undirectedEdgeStatements {
		assertRollbackLeavesGraphUnchanged(t, eng2, base, stmt)
	}
}
