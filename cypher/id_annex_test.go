package cypher_test

// id_annex_test.go — WAL v2 step 3 (rmp #3021 A, docs/design-wal-v2.md §9 step 3):
// the id() a Cypher CREATE returns is the id the node has after a restart.
//
// Layer: short.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

func annexOpen(t *testing.T, dir string) (*store.Opened[string, float64], *cypher.Engine) {
	t.Helper()
	o, err := store.Open[string, float64](dir, store.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	return o, cypher.NewEngineWithOpened(o)
}

func annexClose(t *testing.T, o *store.Opened[string, float64], eng *cypher.Engine) {
	t.Helper()
	_ = eng.Close()
	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// annexIDs runs q in a read and returns tag -> id(n) for every row (columns: tag, id).
func annexIDs(t *testing.T, eng *cypher.Engine, q string) map[string]int64 {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	out := map[string]int64{}
	for res.Next() {
		tag, _ := res.ValueAt(0).(expr.StringValue)
		id, _ := res.ValueAt(1).(expr.IntegerValue)
		out[string(tag)] = int64(id)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return out
}

// TestAnnex_CypherCreateIDSurvivesRestart: `CREATE (n) RETURN id(n)` — with
// rolled-back CREATEs interleaved, so a replay that interned only committed keys
// would shift ids — returns the id the node still has after a restart, with and
// without a checkpoint in between.
func TestAnnex_CypherCreateIDSurvivesRestart(t *testing.T) {
	t.Parallel()
	for _, withCheckpoint := range []bool{false, true} {
		t.Run(fmt.Sprintf("checkpoint=%v", withCheckpoint), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			o, eng := annexOpen(t, dir)
			ctx := context.Background()
			want := map[string]int64{}
			const rounds = 300
			for i := range rounds {
				// A rolled-back CREATE first: it takes a slot the committed one
				// after it then follows.
				tx, err := eng.BeginTx(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := annexDrain(tx.Exec(fmt.Sprintf("CREATE (:R {tag: 'r%d'})", i), nil)); err != nil {
					t.Fatal(err)
				}
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
				tag := fmt.Sprintf("c%d", i)
				ids := mintInts(t, eng, fmt.Sprintf("CREATE (n:C {tag: '%s'}) RETURN id(n)", tag))
				want[tag] = ids[0]
				if withCheckpoint && i == rounds/2 {
					var unused sync.Mutex
					cp := checkpoint.New(checkpoint.Config{Dir: dir}, o.Graph(), o.WAL(), &unused,
						checkpoint.WithCommitSerialiser[string, float64](o.Store().RunUnderCommitLock),
						checkpoint.WithMapperCodec[string, float64](txn.NewStringCodec()),
						checkpoint.WithWeightCodec[string, float64](txn.NewFloat64WeightCodec()),
						checkpoint.WithConstraintSpecs[string, float64](eng.ConstraintSpecsForSnapshot),
						checkpoint.WithIndexSpecs[string, float64](eng.IndexSpecsForSnapshot))
					if err := cp.RunCheckpoint(); err != nil {
						t.Fatalf("RunCheckpoint: %v", err)
					}
				}
			}
			annexClose(t, o, eng)
			o2, eng2 := annexOpen(t, dir)
			defer annexClose(t, o2, eng2)
			got := annexIDs(t, eng2, "MATCH (n:C) RETURN n.tag, id(n)")
			moved := 0
			for tag, id := range want {
				if got[tag] != id {
					moved++
					if moved <= 3 {
						t.Errorf("%s: id %d before the restart, %d after", tag, id, got[tag])
					}
				}
			}
			if moved > 0 || len(got) != len(want) {
				t.Fatalf("%d of %d ids moved across the restart (%d nodes after)", moved, len(want), len(got))
			}
			if r := annexIDs(t, eng2, "MATCH (n:R) RETURN n.tag, id(n)"); len(r) != 0 {
				t.Fatalf("%d rolled-back nodes present after the restart", len(r))
			}
		})
	}
}

// TestAnnex_CypherStatementUndoKeepsIDs: a statement that fails after creating a
// node is undone by the statement-level undo log — in an autocommit statement, and
// inside an explicit transaction that is then rolled back. The committed CREATEs
// around them keep their ids across a restart and no undone node is present.
func TestAnnex_CypherStatementUndoKeepsIDs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	o, eng := annexOpen(t, dir)
	ctx := context.Background()
	want := map[string]int64{}
	for i := range 100 {
		a := fmt.Sprintf("a%d", i)
		want[a] = mintInts(t, eng, fmt.Sprintf("CREATE (n:A {tag: '%s'}) RETURN id(n)", a))[0]
		// Autocommit: creates its node, then fails at run time: undone.
		if res, err := eng.RunInTx(ctx, fmt.Sprintf("CREATE (m:U {tag: 'u%d'}) RETURN 1 / 0", i), nil); err == nil {
			if _, derr := annexDrain(res, nil); derr == nil {
				t.Fatal("the failing autocommit statement did not fail")
			}
		}
		// Explicit: a CREATE, then a failing statement that undoes its own CREATE
		// and poisons the transaction, which is rolled back.
		tx, err := eng.BeginTx(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := annexDrain(tx.Exec(fmt.Sprintf("CREATE (:U {tag: 'x%d'})", i), nil)); err != nil {
			t.Fatal(err)
		}
		if _, err := annexDrain(tx.Exec(fmt.Sprintf("CREATE (m:U {tag: 'y%d'}) RETURN 1 / 0", i), nil)); err == nil {
			t.Fatal("the failing explicit statement did not fail")
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		c := fmt.Sprintf("c%d", i)
		want[c] = mintInts(t, eng, fmt.Sprintf("CREATE (n:A {tag: '%s'}) RETURN id(n)", c))[0]
	}
	annexClose(t, o, eng)
	o2, eng2 := annexOpen(t, dir)
	defer annexClose(t, o2, eng2)
	got := annexIDs(t, eng2, "MATCH (n:A) RETURN n.tag, id(n)")
	moved := 0
	for tag, id := range want {
		if got[tag] != id {
			moved++
			if moved <= 3 {
				t.Errorf("%s: id %d before the restart, %d after", tag, id, got[tag])
			}
		}
	}
	if moved > 0 || len(got) != len(want) {
		t.Fatalf("%d of %d ids moved across the restart (%d nodes after)", moved, len(want), len(got))
	}
	if u := annexIDs(t, eng2, "MATCH (n:U) RETURN n.tag, id(n)"); len(u) != 0 {
		t.Fatalf("%d undone nodes present after the restart", len(u))
	}
}

// annexDrain drains a result whose first column is an integer and closes it.
func annexDrain(res *cypher.Result, err error) ([]int64, error) {
	if err != nil {
		return nil, err
	}
	var out []int64
	for res.Next() {
		if v, ok := res.ValueAt(0).(expr.IntegerValue); ok {
			out = append(out, int64(v))
		}
	}
	rerr := res.Err()
	if cerr := res.Close(); rerr == nil {
		rerr = cerr
	}
	return out, rerr
}
