package cypher_test

// index_noop_write_2989_test.go — regression gate for rmp #2989.
//
// A write that changes nothing — a SET to the value the node already stores, a
// REMOVE of a label the node does not carry — claims nothing in its store, so it
// conflicts with no concurrent writer of the same node. Its index change was
// still fanned out at commit with its own payload: after a peer's commit changed
// the node, the no-op SET re-indexed the value the node had given up, and the
// no-op REMOVE deleted the entry the peer's label add had made. Afterwards the
// index disagreed with the graph for good. Both bound index kinds shared the
// rule, so both are driven.
//
// The oracle is the index's CONTENT, read through its own Lookup, against a scan
// of the graph: a planned range seek keeps the full predicate as a residual
// filter, which hides a stale entry, and is not planned at all on a graph this
// small.

import (
	"context"
	"testing"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// noopIndexKind is one bound index kind on (:L, prop).
type noopIndexKind struct {
	name, index, prop string
	// stored and other are two values of the indexed property.
	stored, other string
}

var noopIndexKinds = []noopIndexKind{
	{name: "hash", index: "ix_s", prop: "s", stored: "s1", other: "s2"},
	{name: "btree", index: "ix_b", prop: "b", stored: "b1", other: "b2"},
}

// noopFixture seeds node 0 carrying :L and node 1 without it, both storing the
// kinds' stored values, and node 2, and creates both bound indexes on (:L).
//
// The transaction under test also writes node 2 for real: one that versions
// nothing publishes nothing and fans nothing out, so a lone no-op cannot reach
// the defect.
func noopFixture(t *testing.T) (*cypher.Engine, *lpg.Graph[string, float64]) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })
	for _, q := range []string{
		"CREATE (:Item:L {id:0, s:'s1', b:'b1'}), (:Item {id:1, s:'s1', b:'b1'}), (:Item {id:2})",
		"CREATE INDEX ix_s FOR (n:L) ON (n.s)",
		"CREATE INDEX ix_b FOR (n:L) ON (n.b) OPTIONS {indexType: 'btree'}",
	} {
		noopRun(t, eng, q)
	}
	return eng, g
}

func noopDrain(t *testing.T, q string, res *cypher.Result, err error) [][]expr.Value {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	var rows [][]expr.Value
	for res.Next() {
		row := make([]expr.Value, len(res.Columns()))
		for i := range row {
			row[i] = res.ValueAt(i)
		}
		rows = append(rows, row)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return rows
}

func noopRun(t *testing.T, eng *cypher.Engine, q string) {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), q, nil)
	noopDrain(t, q, res, err)
}

func noopTxRun(t *testing.T, tx *cypher.ExplicitTx, q string) {
	t.Helper()
	res, err := tx.Exec(q, nil)
	noopDrain(t, q, res, err)
}

// requireIndexMatchesGraph asserts that the graph holds want :L nodes with the
// value v, and that the index holds exactly as many under v.
func requireIndexMatchesGraph(t *testing.T, eng *cypher.Engine, g *lpg.Graph[string, float64],
	k *noopIndexKind, v string, want uint64,
) {
	t.Helper()
	q := "MATCH (m:Item) WHERE 'L' IN labels(m) AND m." + k.prop + " + '' = $v RETURN count(m)"
	res, err := eng.Run(context.Background(), q, map[string]expr.Value{"v": expr.StringValue(v)})
	rows := noopDrain(t, q, res, err)
	if scan, ok := rows[0][0].(expr.IntegerValue); !ok || uint64(scan) != want {
		t.Fatalf("setup: %v :L nodes hold %s = %q, want %d", rows[0][0], k.prop, v, want)
	}
	sub, err := g.IndexManager().GetIndex(k.index)
	if err != nil {
		t.Fatal(err)
	}
	ix, ok := sub.(interface {
		Lookup(string) *roaring64.Bitmap
	})
	if !ok {
		t.Fatalf("index %s (%T) has no string Lookup", k.index, sub)
	}
	if got := ix.Lookup(v).GetCardinality(); got != want {
		t.Errorf("%s index holds %d nodes under %q, the graph %d: the index drifted (rmp #2989)",
			k.name, got, v, want)
	}
}

// TestIndexFanOut_NoOpSetAfterPeerCommit_2989: T sets the property to the value
// the node already stores, a peer commits a different value, T commits. Before
// the fix T's commit re-inserted the stored value, so the index kept the node
// under a value it no longer carries.
func TestIndexFanOut_NoOpSetAfterPeerCommit_2989(t *testing.T) {
	t.Parallel()
	for _, k := range noopIndexKinds {
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			eng, g := noopFixture(t)
			tx, err := eng.BeginTx(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			noopTxRun(t, tx, "MATCH (n:Item {id:2}) SET n.z = 1")
			noopTxRun(t, tx, "MATCH (n:Item {id:0}) SET n."+k.prop+" = '"+k.stored+"'")
			noopRun(t, eng, "MATCH (n:Item {id:0}) SET n."+k.prop+" = '"+k.other+"'")
			if err := tx.Commit(); err != nil {
				t.Fatalf("setup: the no-op transaction did not commit: %v", err)
			}
			requireIndexMatchesGraph(t, eng, g, &k, k.other, 1)
			requireIndexMatchesGraph(t, eng, g, &k, k.stored, 0)
		})
	}
}

// TestIndexFanOut_NoOpLabelRemoveAfterPeerAdd_2989: T removes :L from a node
// that does not carry it, a peer adds :L, T commits. Before the fix T's commit
// deleted the peer's entry, so the index lost a node that carries the label and
// the value.
func TestIndexFanOut_NoOpLabelRemoveAfterPeerAdd_2989(t *testing.T) {
	t.Parallel()
	for _, k := range noopIndexKinds {
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			eng, g := noopFixture(t)
			tx, err := eng.BeginTx(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			noopTxRun(t, tx, "MATCH (n:Item {id:2}) SET n.z = 1")
			noopTxRun(t, tx, "MATCH (n:Item {id:1}) REMOVE n:L")
			noopRun(t, eng, "MATCH (n:Item {id:1}) SET n:L")
			if err := tx.Commit(); err != nil {
				t.Fatalf("setup: the no-op transaction did not commit: %v", err)
			}
			requireIndexMatchesGraph(t, eng, g, &k, k.stored, 2)
		})
	}
}
