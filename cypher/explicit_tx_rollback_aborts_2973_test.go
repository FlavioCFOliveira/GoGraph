package cypher_test

// explicit_tx_rollback_aborts_2973_test.go — regression gate for rmp #2973.
//
// ROLLBACK of an explicit transaction ended its versioned transaction as a
// COMMIT: the physical undo replay restored the values, and the transaction's
// record — carrying both its writes and their inverses — was then PUBLISHED. A
// published record is a committed write after the start of every transaction
// that began earlier, so such a transaction writing the same object afterwards
// was refused by first-updater-wins ("the newest version is not visible to this
// transaction") although nothing it could see had changed; and the substrate
// counted the rollback as a commit. A rolled-back transaction must ABORT: no
// committed version, Aborts +1, Commits unchanged.
//
// The same holds for a Commit that is REFUSED and rolls the transaction back
// itself — the NOT NULL branch is driven here — because nothing it wrote ever
// became durable or visible.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestExplicitTxRollbackAborts_2973(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		ddl    string // optional schema statement run first
		extra  string // optional second statement of the ending transaction
		finish func(tx *cypher.ExplicitTx) error
	}{
		{
			name:   "Rollback",
			finish: (*cypher.ExplicitTx).Rollback,
		},
		{
			name:  "Commit refused by a NOT NULL constraint",
			ddl:   "CREATE CONSTRAINT item_x_nn FOR (n:Item) REQUIRE n.x IS NOT NULL",
			extra: "CREATE (:Item {name: 'm'})", // violates the constraint at COMMIT
			finish: func(tx *cypher.ExplicitTx) error {
				err := tx.Commit()
				if err == nil {
					return errors.New("Commit succeeded; the NOT NULL refusal was expected")
				}
				return nil
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			g := lpg.New[string, float64](adjlist.Config{})
			e := cypher.NewEngine(g)
			if c.ddl != "" {
				res, err := e.Run(ctx, c.ddl, nil)
				if err != nil {
					t.Fatalf("schema: %v", err)
				}
				_ = res.Close() // DDL result carries nothing to read
			}
			if _, err := e.RunAny(ctx, "CREATE (:Item {name: 'n', x: 0})", nil); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			t2, err := e.BeginTx(ctx) // the OLDER snapshot
			if err != nil {
				t.Fatalf("BeginTx T2: %v", err)
			}
			t1, err := e.BeginTx(ctx)
			if err != nil {
				t.Fatalf("BeginTx T1: %v", err)
			}
			if _, err := t1.Exec("MATCH (n:Item {name: 'n'}) SET n.x = 1", nil); err != nil {
				t.Fatalf("T1 write: %v", err)
			}
			if c.extra != "" {
				if _, err := t1.Exec(c.extra, nil); err != nil {
					t.Fatalf("T1 %s: %v", c.extra, err)
				}
			}
			before := g.MVCCStats().Write
			if err := c.finish(t1); err != nil {
				t.Fatalf("T1 finish: %v", err)
			}
			after := g.MVCCStats().Write
			if after.Commits != before.Commits {
				t.Errorf("rollback counted as a commit: Commits %d -> %d", before.Commits, after.Commits)
			}
			if after.Aborts != before.Aborts+1 {
				t.Errorf("rollback not counted as an abort: Aborts %d -> %d, want +1", before.Aborts, after.Aborts)
			}
			g.ReclaimNow()
			if _, err := t2.Exec("MATCH (n:Item {name: 'n'}) SET n.x = 2", nil); err != nil {
				t.Fatalf("T2 write after T1's rollback: %v", err)
			}
			if err := t2.Commit(); err != nil {
				t.Fatalf("T2 Commit: %v", err)
			}
			res, err := e.RunAny(ctx, "MATCH (n:Item) RETURN collect(n.name + '=' + toString(n.x)) AS v", nil)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			defer func() { _ = res.Close() }() // read-only use; Close error carries nothing here
			if !res.Next() {
				t.Fatalf("read back: no row (err %v)", res.Err())
			}
			if got := res.ValueAt(0).String(); got != `["n=2"]` {
				t.Fatalf("state after T2 committed: %s, want [\"n=2\"]", got)
			}
		})
	}
}
