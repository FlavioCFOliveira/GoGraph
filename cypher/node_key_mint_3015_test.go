package cypher_test

// node_key_mint_3015_test.go — rmp #3015 (catalogue GG07): a CREATE or MERGE
// must never be handed a node key the graph already holds.
//
// Synthetic node keys come from a process-wide counter seeded ONCE per process,
// from the first graph a write operator runs against. A graph whose synthetic
// keys were minted elsewhere — a store persisted by an earlier process and
// opened after the seed ran, the second of two stores in one process — holds
// keys at and above the counter's next values. Before the fix, interning such a
// key returned the existing node, so the CREATE overwrote a committed node and
// two acknowledged CREATEs named one node.
//
// The test reproduces that shape in one process: it reads the counter's next
// value through a throwaway CREATE, writes the next window of "__cx_<hex>" and
// "__cx_merge_<hex>" keys through the lpg / txn API (exactly what a store
// written by another process recovers to), and then CREATEs and MERGEs.
//
// Not parallel: it reads and then occupies the counter's next values, and a
// parallel test minting keys in between would move the counter past the window
// and make the arm vacuous. Top-level parallel tests do not run while a
// sequential test does.

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// mintWindow is how many counter values the test occupies ahead of the counter.
const mintWindow = 64

func newMintGraph() *lpg.Graph[string, float64] {
	return lpg.New[string, float64](adjlist.Config{})
}

// mintInts runs q and returns column 0 of every row as an integer.
func mintInts(t *testing.T, eng *cypher.Engine, q string) []int64 {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	var out []int64
	for res.Next() {
		v, ok := res.ValueAt(0).(expr.IntegerValue)
		if !ok {
			t.Fatalf("%s: column 0 is %T, want an integer", q, res.ValueAt(0))
		}
		out = append(out, int64(v))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return out
}

// mintOne runs q and returns its single integer.
func mintOne(t *testing.T, eng *cypher.Engine, q string) int64 {
	t.Helper()
	got := mintInts(t, eng, q)
	if len(got) != 1 {
		t.Fatalf("%s: %d rows, want 1", q, len(got))
	}
	return got[0]
}

// nextCounterValue returns the value the process-wide counter will hand out
// next, read from the key a throwaway CREATE was given.
func nextCounterValue(t *testing.T) uint64 {
	t.Helper()
	g := newMintGraph()
	defer func() { _ = g.Close() }()
	id := mintOne(t, cypher.NewEngine(g), `CREATE (n) RETURN id(n)`)
	key, ok := g.AdjList().Mapper().Resolve(graph.NodeID(id))
	if !ok {
		t.Fatalf("node %d has no key", id)
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(key, "__cx_"), 16, 64)
	if err != nil {
		t.Fatalf("key %q is not __cx_<hex>: %v", key, err)
	}
	return v + 1
}

// foreignKeys are the keys another process would have minted from next onwards.
func foreignKeys(next uint64) []string {
	keys := make([]string, 0, 2*mintWindow)
	for k := uint64(0); k < mintWindow; k++ {
		h := strconv.FormatUint(next+k, 16)
		keys = append(keys, "__cx_"+h, "__cx_merge_"+h)
	}
	return keys
}

// assertDistinctNodes holds eng to: every foreign node untouched, every CREATE
// and MERGE a node of its own.
func assertDistinctNodes(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	created := mintInts(t, eng, `UNWIND range(1, 8) AS i CREATE (n:New {i: i}) RETURN id(n)`)
	merged := mintInts(t, eng, `UNWIND range(1, 4) AS i MERGE (m:MNew {i: i}) RETURN id(m)`)
	mintInts(t, eng, `MERGE (a:MPA {k: 1})-[:R]->(b:MPB {k: 1}) RETURN 1`)

	foreign := 2 * mintWindow
	if got := mintOne(t, eng, `MATCH (n) WHERE n.owner = 'foreign' AND size(labels(n)) = 0
		AND size(keys(n)) = 2 RETURN count(n)`); got != int64(foreign) {
		t.Errorf("untouched foreign nodes = %d, want %d: a CREATE or MERGE took over a node "+
			"whose key the graph already held", got, foreign)
	}
	ids := make(map[int64]bool, len(created)+len(merged))
	for _, id := range append(created, merged...) {
		if ids[id] {
			t.Errorf("id %d was returned by two committed CREATE/MERGE rows", id)
		}
		ids[id] = true
	}
	for _, q := range []struct {
		q    string
		want int64
	}{
		{`MATCH (n:New) RETURN count(DISTINCT n.i)`, 8},
		{`MATCH (n:MNew) RETURN count(DISTINCT n.i)`, 4},
		{`MATCH (:MPA {k: 1})-[:R]->(:MPB {k: 1}) RETURN count(*)`, 1},
		{`MATCH (n) RETURN count(n)`, int64(foreign + 8 + 4 + 2)},
		{`MATCH (n) RETURN count(DISTINCT id(n))`, int64(foreign + 8 + 4 + 2)},
		{`MATCH (n) RETURN count(DISTINCT elementId(n))`, int64(foreign + 8 + 4 + 2)},
	} {
		if got := mintOne(t, eng, q.q); got != q.want {
			t.Errorf("%s = %d, want %d", q.q, got, q.want)
		}
	}
}

// TestNodeKeyMintSkipsHeldKeys_3015 is the regression test for rmp #3015.
func TestNodeKeyMintSkipsHeldKeys_3015(t *testing.T) { //nolint:paralleltest // occupies the process-wide counter's next values
	t.Run("memory", func(t *testing.T) { //nolint:paralleltest // see the parent
		next := nextCounterValue(t)
		g := newMintGraph()
		defer func() { _ = g.Close() }()
		if err := g.ApplyVersioned(func(tx lpg.WriteTx) error {
			w := g.Writer(tx)
			for _, k := range foreignKeys(next) {
				if err := w.AddNode(k); err != nil {
					return err
				}
				if err := w.SetNodeProperty(k, "owner", lpg.StringValue("foreign")); err != nil {
					return err
				}
				if err := w.SetNodeProperty(k, "k", lpg.StringValue(k)); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("write foreign keys: %v", err)
		}
		assertDistinctNodes(t, cypher.NewEngine(g))
	})

	t.Run("store", func(t *testing.T) { //nolint:paralleltest // see the parent
		next := nextCounterValue(t)
		dir := t.TempDir()
		opts := store.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
		o, err := store.Open[string, float64](dir, opts)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		tx := o.Store().Begin()
		for _, k := range foreignKeys(next) {
			if err := tx.AddNode(k); err != nil {
				t.Fatalf("AddNode %s: %v", k, err)
			}
			if err := tx.SetNodeProperty(k, "owner", lpg.StringValue("foreign")); err != nil {
				t.Fatalf("SetNodeProperty %s: %v", k, err)
			}
			if err := tx.SetNodeProperty(k, "k", lpg.StringValue(k)); err != nil {
				t.Fatalf("SetNodeProperty %s: %v", k, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if err := o.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		// Reopen: the foreign keys now come from recovery, as they would for a
		// store another process wrote.
		o, err = store.Open[string, float64](dir, opts)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer func() { _ = o.Close() }()
		assertDistinctNodes(t, cypher.NewEngineWithOpened(o))
	})
}
