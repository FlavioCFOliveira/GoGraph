package cypher_test

// phantom_node_aborted_delete_3001_test.go — regression gate for rmp #3001.
//
// A read transaction must not see a node created after its snapshot. Once
// another transaction's DETACH DELETE of that node rolled back, it did: the
// rollback's undo revives the node with a birth record that overwrote the
// committed birth, the abort deleted both of its records, and the reader fell
// back to the present tombstone bitmap. `MATCH (n) RETURN count(n)` went from 4
// to 5 inside one read transaction, the extra row a node with no properties.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestReadTx_RolledBackDeleteOfYoungNodeStaysInvisible_3001(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"detach delete", []string{"MATCH (h:Hub {id:9}) DETACH DELETE h"}},
		{"detach delete then create", []string{"MATCH (h:Hub {id:9}) DETACH DELETE h", "CREATE (:Hub {id:9})"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := lpg.New[string, float64](adjlist.Config{})
			eng := cypher.NewEngine(g)
			t.Cleanup(func() { _ = eng.Close() })
			run := func(q string) {
				t.Helper()
				res, err := eng.RunInTx(ctx, q, nil)
				if err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				_ = res.Close()
			}
			run("UNWIND range(0, 3) AS i CREATE (:Hub {id:i})")

			r, err := eng.BeginReadTx(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Rollback() })
			read := func() string {
				t.Helper()
				var s string
				for _, q := range []string{
					"MATCH (n) RETURN count(n) AS c",
					"MATCH (n) RETURN n.id AS id ORDER BY id",
				} {
					res, err := r.Exec(q, nil)
					if err != nil {
						t.Fatal(err)
					}
					for res.Next() {
						s += res.ValueAt(0).String() + ","
					}
					_ = res.Close()
					s += ";"
				}
				return s
			}
			want := read()

			// Created AFTER the reader's snapshot.
			run("CREATE (:Hub {id:9})")
			if got := read(); got != want {
				t.Fatalf("a committed create leaked into the read transaction: %s, want %s", got, want)
			}

			tx, err := eng.BeginTx(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, q := range tc.stmts {
				res, err := tx.Exec(q, nil)
				if err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				_ = res.Close()
			}
			if got := read(); got != want {
				t.Errorf("in flight: %s, want %s", got, want)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if got := read(); got != want {
				t.Errorf("after the rollback the read transaction sees a node created after its snapshot: %s, want %s", got, want)
			}
			g.ReclaimNow()
			if got := read(); got != want {
				t.Errorf("after reclamation: %s, want %s", got, want)
			}
		})
	}
}
