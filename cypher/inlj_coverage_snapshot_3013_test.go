package cypher_test

// inlj_coverage_snapshot_3013_test.go — regression gate for rmp #3013.
//
// The index nested-loop join proves, at plan time, that the numeric companion
// index covers every node of the inner label scan, and then answers a
// non-numeric join key with "no rows" without reading the graph. The proof
// compared the PRESENT-state index with the reader's SNAPSHOT count. Inside an
// explicit read transaction, a peer's later commit of a numerically keyed node
// added an index entry the snapshot has no node for, the comparison passed while
// a node the snapshot holds carried a non-numeric value, and that node's row was
// lost: 0 rows where the scan returns 1.
//
// Every scenario runs on the in-memory engine and on the persisted store
// (store.Open).
//
// Layer: short. Race-clean.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store"
)

const inlj3013Query = `UNWIND [{a: "s"}] AS r MATCH (b:P) WHERE b.age = r.a RETURN b.id AS bid`

// inlj3013Engines returns the two engines the scenario runs on.
func inlj3013Engines() map[string]func(t *testing.T) *cypher.Engine {
	return map[string]func(t *testing.T) *cypher.Engine{
		"memory": func(t *testing.T) *cypher.Engine {
			eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true}))
			t.Cleanup(func() { _ = eng.Close() })
			return eng
		},
		"persisted": func(t *testing.T) *cypher.Engine {
			o, err := store.Open(t.TempDir(), openedOptions())
			if err != nil {
				t.Fatalf("store.Open: %v", err)
			}
			eng := cypher.NewEngineWithOpened(o)
			t.Cleanup(func() {
				_ = eng.Close()
				if err := o.Close(); err != nil {
					t.Errorf("store Close: %v", err)
				}
			})
			return eng
		},
	}
}

// inlj3013Rows drains res and returns the bid column.
func inlj3013Rows(t *testing.T, res *cypher.Result, err error) []int64 {
	t.Helper()
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var out []int64
	for res.Next() {
		v, ok := res.Record()["bid"].(expr.IntegerValue)
		if !ok {
			t.Fatalf("bid is %T, want an integer", res.Record()["bid"])
		}
		out = append(out, int64(v))
	}
	if err := res.Close(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	return out
}

// TestINLJ_CoverageIsProvedAtTheReadersSnapshot_3013 builds 200 :P nodes with
// numeric ages except node 7, whose age is the string "s", and an index on
// :P(age). Then an explicit read
// transaction begins, a peer commits (:P {age: 1}), and the read transaction's
// join on the key "s" must still return node 7.
func TestINLJ_CoverageIsProvedAtTheReadersSnapshot_3013(t *testing.T) {
	ctx := context.Background()
	for name, mk := range inlj3013Engines() {
		t.Run(name, func(t *testing.T) {
			eng := mk(t)
			for _, q := range []string{
				`UNWIND range(0, 199) AS i CREATE (:P {id: i, age: i % 20})`,
				`MATCH (n:P {id: 7}) SET n.age = "s"`,
				`CREATE INDEX p_age FOR (x:P) ON (x.age)`,
			} {
				res, err := eng.RunAny(ctx, q, nil)
				if err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				if err := res.Close(); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			// Precondition: the 199 numeric entries do not cover 200 nodes, so the
			// join declines here; the peer's commit below is what makes the
			// present-state count reach the snapshot's.
			res, err := eng.RunAny(ctx, inlj3013Query, nil)
			if got := inlj3013Rows(t, res, err); len(got) != 1 || got[0] != 7 {
				t.Fatalf("autocommit before the peer commit = %v, want [7]", got)
			}

			rtx, err := eng.BeginReadTx(ctx)
			if err != nil {
				t.Fatalf("BeginReadTx: %v", err)
			}
			defer func() { _ = rtx.Commit() }()
			peer, err := eng.RunAny(ctx, `CREATE (:P {id: 999, age: 1})`, nil)
			if err != nil {
				t.Fatalf("peer commit: %v", err)
			}
			if err := peer.Close(); err != nil {
				t.Fatalf("peer commit: %v", err)
			}

			// The read transaction's snapshot holds 200 :P nodes, and the present-state
			// index now holds 200 numeric entries (199 of them plus the peer's), so a
			// coverage proof taken against the present passes for this snapshot. That
			// it is reached is shown by this test failing on the pre-fix code (rmp
			// #3013 record); an autocommit statement here sees 201 nodes and rightly
			// declines, so it cannot show it.
			res, err = rtx.ExecAny(inlj3013Query, nil)
			got := inlj3013Rows(t, res, err)
			if len(got) != 1 || got[0] != 7 {
				t.Fatalf("read transaction after a peer commit = %v, want [7]: the index nested-loop "+
					"join proved coverage against an index its snapshot does not see", got)
			}
		})
	}
}
