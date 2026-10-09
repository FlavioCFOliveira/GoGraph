package cypher_test

// rollback_parallel_edges_test.go — regression tests for rmp #2885: a rolled-back
// explicit transaction must leave the committed graph byte-for-byte unchanged when
// its statements touch endpoint pairs that already hold committed PARALLEL
// relationships.
//
// Two undo inverses addressed the pair instead of the instance:
//
//   - DETACH DELETE removes a node's out-edges in bulk; the undo captured every
//     parallel slot to the same neighbour under the FIRST slot's handle, so the
//     rollback re-added one instance and lost the others.
//   - CREATE (and MERGE's create branch) appends a new parallel instance; the undo
//     removed the FIRST slot of the pair — a committed sibling — and kept the
//     rolled-back instance in its place, so every committed relationship parallel
//     to a rolled-back one read back as the rolled-back type.
//
// Each statement below is run in TWO consecutive rolled-back transactions: the
// second must return exactly the rows of the first, because it runs against the
// committed graph the first rollback left behind.
//
// Layer: short. Engines, graphs and stores are local; the suite is goleak-clean.

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
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

// parallelEdgeSeed builds a directed multigraph whose pairs carry parallel edges
// of mixed types (1→3 holds :U, :U and :T) plus a self-loop (4→4), each with a
// distinct w so every instance is individually identifiable.
var parallelEdgeSeed = []string{
	"UNWIND range(0, 5) AS i CREATE (:S {k: i})",
	"MATCH (a:S), (b:S) WHERE b.k = a.k + 1 CREATE (a)-[:T {w: a.k}]->(b)",
	"MATCH (a:S {k: 1}), (b:S {k: 3}) CREATE (a)-[:U {w: 1}]->(b), (a)-[:U {w: 2}]->(b), (a)-[:T {w: 9}]->(b)",
	"MATCH (a:S {k: 4}) CREATE (a)-[:T {w: 44}]->(a)",
}

// parallelEdgeStatements are rolled back one at a time. The first two were
// already correct before #2885 and are kept as controls; the rest are the
// statements whose rollback corrupted the committed graph.
var parallelEdgeStatements = []string{
	"MATCH (a:S)-[:T]->(b) CREATE (b)-[:T {w: 100}]->(:S {k: 100 + b.k}) RETURN a.k, b.k",
	"MATCH (a:S)-[r:T]->(b) DELETE r RETURN a.k, b.k",
	"MATCH (a:S)-[:T]->(b) DETACH DELETE b RETURN a.k",
	"MATCH (a:S {k: 1}) DETACH DELETE a",
	"MATCH p = (a:S {k: 1})-[:T]->(:S {k: 2}) DETACH DELETE p",
	"MATCH (a:S)-->(b) CREATE (a)-[:V {w: -1}]->(b) RETURN a.k, b.k",
	"MATCH (a:S)-[:U]->(b) MERGE (a)-[:W]->(b) RETURN a.k, b.k",
}

// graphDump returns the whole committed graph — every node and every
// relationship with its identity, type, property and endpoints — as a sorted
// list of rows, so two dumps compare equal only when the graphs are identical.
func graphDump(t *testing.T, eng *cypher.Engine) []string {
	t.Helper()
	ctx := context.Background()
	queries := []string{
		"MATCH (n) RETURN 'node', n.k, labels(n)",
		"MATCH (a)-[r]->(b) RETURN 'rel', id(r), a.k, type(r), r.w, b.k",
	}
	out := make([]string, 0, 32)
	for _, q := range queries {
		res, err := eng.Run(ctx, q, nil)
		out = append(out, collectRows(t, res, err)...)
	}
	slices.Sort(out)
	return out
}

// collectRows drains res into one "|"-joined string per row, sorted.
func collectRows(t *testing.T, res *cypher.Result, err error) []string {
	t.Helper()
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var out []string
	for res.Next() {
		cols := make([]string, len(res.Columns()))
		for i := range cols {
			cols[i] = res.ValueAt(i).String()
		}
		out = append(out, strings.Join(cols, "|"))
	}
	if rerr := res.Err(); rerr != nil {
		_ = res.Close()
		t.Fatalf("drain: %v", rerr)
	}
	if cerr := res.Close(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}
	slices.Sort(out)
	return out
}

// seedParallelEdges commits every seed statement in its own transaction.
func seedParallelEdges(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	for _, q := range parallelEdgeSeed {
		res, err := eng.RunInTx(context.Background(), q, nil)
		collectRows(t, res, err)
	}
}

// rolledBackRows runs stmt inside an explicit transaction, collects its rows,
// and rolls the transaction back.
func rolledBackRows(t *testing.T, eng *cypher.Engine, stmt string) []string {
	t.Helper()
	tx, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	res, err := tx.Exec(stmt, nil)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("Exec(%q): %v", stmt, err)
	}
	rows := collectRows(t, res, nil)
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback(%q): %v", stmt, err)
	}
	return rows
}

// assertRollbackLeavesGraphUnchanged rolls stmt back twice against the
// committed baseline base. It reports every dump that differs from base and a
// second run whose rows differ from the first, with Errorf rather than stopping,
// so a durable caller still reaches its own post-reopen check on a defective
// build.
func assertRollbackLeavesGraphUnchanged(t *testing.T, eng *cypher.Engine, base []string, stmt string) {
	t.Helper()
	first := rolledBackRows(t, eng, stmt)
	if got := graphDump(t, eng); !slices.Equal(got, base) {
		t.Errorf("after rolled-back %q the committed graph changed:\n got  %v\n want %v", stmt, got, base)
	}
	second := rolledBackRows(t, eng, stmt)
	if !slices.Equal(second, first) {
		t.Errorf("second rolled-back run of %q returned different rows:\n got  %v\n want %v", stmt, second, first)
	}
	if got := graphDump(t, eng); !slices.Equal(got, base) {
		t.Errorf("after the second rolled-back %q the committed graph changed:\n got  %v\n want %v", stmt, got, base)
	}
}

// TestRollback_ParallelEdges_InMemory_2885 covers the in-memory engine over a
// directed multigraph, the openCypher storage model. Every statement runs in its
// own subtest over a freshly seeded graph, so each verdict belongs to that
// statement alone and is not inherited from an earlier one's damage.
//
// The final subtest replays the whole list, in order, over ONE graph — the
// original reproduction. It is not redundant with the isolated subtests: a
// rollback re-appends restored slots at the end of their adjacency entries, so
// earlier statements change the order in which DETACH DELETE later visits its
// victims, and `DETACH DELETE b` reaches node 1's parallel out-edges through the
// bulk path only in that order (in isolation node 3 is visited first and its
// in-edge sweep removes them one by one, a path that was already correct).
func TestRollback_ParallelEdges_InMemory_2885(t *testing.T) {
	newSeeded := func(t *testing.T) (*cypher.Engine, []string) {
		t.Helper()
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		eng := cypher.NewEngine(g)
		seedParallelEdges(t, eng)
		base := graphDump(t, eng)
		if len(base) == 0 {
			t.Fatal("seed produced an empty graph")
		}
		return eng, base
	}
	for _, stmt := range parallelEdgeStatements {
		t.Run(stmt, func(t *testing.T) {
			eng, base := newSeeded(t)
			assertRollbackLeavesGraphUnchanged(t, eng, base, stmt)
		})
	}
	t.Run("sequence", func(t *testing.T) {
		eng, base := newSeeded(t)
		for _, stmt := range parallelEdgeStatements {
			assertRollbackLeavesGraphUnchanged(t, eng, base, stmt)
		}
	})
}

// parallelEdgeStoreOpts / parallelEdgeRecOpts are the string-key, float64-weight
// codec wiring the durable tests share. A fresh directory recovers as a directed
// multigraph, the recovery default; the test asserts it rather than assuming it.
func parallelEdgeStoreOpts() txn.Options[string, float64] {
	return txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
}

func parallelEdgeRecOpts() recovery.Options[string, float64] {
	return recovery.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
}

// TestRollback_ParallelEdges_Durable_2885 covers the WAL-backed engine and what
// recovery returns afterwards. The WAL never carries a rolled-back transaction,
// so WAL replay alone cannot see the defect; a CHECKPOINT can, because it
// snapshots the in-memory graph. The durable graph is therefore closed through
// [store.DB] with a final checkpoint, reopened from that snapshot, and compared
// with the baseline.
func TestRollback_ParallelEdges_Durable_2885(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	open := func() (*cypher.Engine, *lpg.Graph[string, float64], *wal.Writer) {
		res, err := recovery.Open[string, float64](dir, parallelEdgeRecOpts())
		if err != nil {
			t.Fatalf("recovery.Open: %v", err)
		}
		w, err := wal.Open(walPath)
		if err != nil {
			t.Fatalf("wal.Open: %v", err)
		}
		st := txn.NewStoreWithOptions[string, float64](res.Graph, w, parallelEdgeStoreOpts())
		return cypher.NewEngineWithStore(st), res.Graph, w
	}

	eng, g, w := open()
	if !g.AdjList().Multigraph() || !g.AdjList().Directed() {
		t.Fatalf("recovered graph is not a directed multigraph: directed=%v multigraph=%v",
			g.AdjList().Directed(), g.AdjList().Multigraph())
	}
	seedParallelEdges(t, eng)
	base := graphDump(t, eng)
	if len(base) == 0 {
		t.Fatal("seed produced an empty graph")
	}
	for _, stmt := range parallelEdgeStatements {
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

	eng2, _, w2 := open()
	defer func() {
		if err := w2.Close(); err != nil {
			t.Errorf("wal.Close: %v", err)
		}
	}()
	if got := graphDump(t, eng2); !slices.Equal(got, base) {
		t.Fatalf("after checkpoint + reopen the recovered graph differs from the committed one:\n got  %v\n want %v", got, base)
	}
}
