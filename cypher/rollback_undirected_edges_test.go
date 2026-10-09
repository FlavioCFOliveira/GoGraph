package cypher_test

// rollback_undirected_edges_test.go — regression tests for rmp #2886: a
// rolled-back transaction that removes relationships reached against their
// stored direction must leave the committed graph byte-for-byte unchanged.
//
// A relationship's type and properties are stored under its creation order
// (a→b). A removal reached from the relationship's END node — an undirected or
// incoming pattern anchored there, or DETACH DELETE of the end node — must
// still capture the undo pre-image under the stored order, or the rollback
// re-adds a reversed relationship with no type and no properties. Storage is a
// directed multigraph (rmp #3072); the undirected side is the pattern only.
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

// undirectedEdgeStatements are every #2885 statement plus the removals that
// reach a relationship from its end node: node 3 is the end node of four
// committed relationships, three of them parallel, so an undirected or incoming
// pattern anchored on it binds them against their stored direction. Node 4
// carries the self-loop, which both directions reach.
var undirectedEdgeStatements = append(slices.Clone(parallelEdgeStatements),
	"MATCH (a:S {k: 1})-[r]-(b) DELETE r RETURN b.k",
	"MATCH (a:S {k: 3})-[r]-(b) DELETE r RETURN b.k",
	"MATCH (a:S {k: 3})<-[r]-(b) DELETE r RETURN b.k",
	"MATCH (a:S {k: 3})-[r:U]-(b) DELETE r RETURN b.k",
	"MATCH (a:S {k: 3}) DETACH DELETE a",
	"MATCH (a:S {k: 4}) DETACH DELETE a",
	"MATCH (a:S {k: 4})-[r]-(b) DELETE r RETURN b.k",
	"MATCH (a:S {k: 3})-[r]-(b) SET r.z = 1 RETURN b.k",
)

// TestRollback_UndirectedEdges_InMemory_2886 covers the in-memory engine. Each
// statement runs in its own subtest over a freshly
// seeded graph, and the final subtest replays the whole list over ONE graph,
// for the reason [TestRollback_ParallelEdges_InMemory_2885] gives.
func TestRollback_UndirectedEdges_InMemory_2886(t *testing.T) {
	newSeeded := func(t *testing.T) (*cypher.Engine, []string) {
		t.Helper()
		g := lpg.New[string, float64](adjlist.Config{})
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

// TestRollback_UndirectedEdges_Durable_2886 covers the WAL-backed engine and
// what recovery returns afterwards. After a final checkpoint the reopen must
// return the committed graph, the statements are rolled back again over the
// relationships recovery rebuilt, and the graph is compared with the baseline
// once more.
func TestRollback_UndirectedEdges_Durable_2886(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{})
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
