package cypher

// constraint_straddle_release_before_test.go — a transaction that releases a
// value before a UNIQUE constraint exists and takes it again after the
// constraint is created commits when its final state is valid (rmp #2948; see
// exec.ConstraintRegistry.adoptStraddledRelease). A value held by anything the
// transaction did not release is still refused at the statement.
//
// Layer: short.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

const straddleUniqueDDL = "CREATE CONSTRAINT us FOR (n:L) REQUIRE n.s IS UNIQUE"

// TestConstraintStraddle_ReleaseBeforeReserveAfterCommits: T frees 'a' from
// node 1 before the constraint exists and puts it on a new node after; T alone
// holds 'a' at the end, so T commits and 'a' stays reserved for the new node.
// Before the fix the CREATE was refused at the statement, because no release
// mark was recorded before the constraint existed.
func TestConstraintStraddle_ReleaseBeforeReserveAfterCommits(t *testing.T) {
	releases := []struct{ name, q string }{
		{"label", `MATCH (n {id: 1}) REMOVE n:L`},
		{"value", `MATCH (n {id: 1}) SET n.s = 'x'`},
		{"property", `MATCH (n {id: 1}) REMOVE n.s`},
		{"life", `MATCH (n {id: 1}) DETACH DELETE n`},
	}
	reserves := []struct{ name, q string }{
		{"newNode", `CREATE (:L {id: 9, s: 'a'})`},
		{"node2", `MATCH (n {id: 2}) SET n:L, n.s = 'a'`},
	}
	for _, rel := range releases {
		for _, res := range reserves {
			t.Run(rel.name+"/"+res.name, func(t *testing.T) {
				straddleWirings(t, func(t *testing.T, walBacked bool) {
					eng, _, _ := straddleEngine(t, walBacked)
					if err := straddleRun(eng, `CREATE (:L {id: 1, s: 'a'}), ({id: 2, s: 'b'})`); err != nil {
						t.Fatal(err)
					}
					tx, err := eng.BeginTx(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					straddleExec(t, tx, rel.q)
					if err := straddleRun(eng, straddleUniqueDDL); err != nil {
						t.Fatalf("CREATE CONSTRAINT: %v", err)
					}
					if err := txExecErr(tx, res.q); err != nil {
						_ = tx.Rollback()
						t.Fatalf("%s after %s was refused although the transaction alone holds 'a': %v", res.q, rel.q, err)
					}
					if err := tx.Commit(); err != nil {
						t.Fatalf("commit refused although the final state is valid: %v", err)
					}
					if n := straddleNodes(t, eng, "a"); n != 1 {
						t.Fatalf("%d :L nodes hold s = 'a', want 1", n)
					}
					if err := straddleRun(eng, `CREATE (:L {s: 'a'})`); !errors.Is(err, exec.ErrConstraintViolation) {
						t.Fatalf("a second node with s = 'a' returned %v, want ErrConstraintViolation", err)
					}
				})
			})
		}
	}
}

// TestConstraintStraddle_ReleaseBeforeStillRefusesOtherHolder: the deferral
// covers only a value the transaction itself released. T releases 'a' before
// the constraint exists, then takes 'b', which a committed node T never touched
// holds, and 'x', which a live peer reserved after the constraint was created:
// both are refused at the statement, as before (retryable and conservative).
func TestConstraintStraddle_ReleaseBeforeStillRefusesOtherHolder(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		ctx := context.Background()
		eng, _, _ := straddleEngine(t, walBacked)
		if err := straddleRun(eng, `CREATE (:L {id: 1, s: 'a'}), (:L {id: 2, s: 'b'})`); err != nil {
			t.Fatal(err)
		}
		tx, err := eng.BeginTx(ctx)
		if err != nil {
			t.Fatal(err)
		}
		straddleExec(t, tx, `MATCH (n {id: 1}) REMOVE n:L`)
		if err := straddleRun(eng, straddleUniqueDDL); err != nil {
			t.Fatalf("CREATE CONSTRAINT: %v", err)
		}
		if err := txExecErr(tx, `CREATE (:L {s: 'b'})`); !errors.Is(err, exec.ErrConstraintViolation) {
			t.Fatalf("taking 'b', held by a committed node T never touched, returned %v, want ErrConstraintViolation", err)
		}

		_ = tx.Rollback()

		eng2, _, _ := straddleEngine(t, walBacked)
		if err := straddleRun(eng2, `CREATE (:L {id: 1, s: 'a'})`); err != nil {
			t.Fatal(err)
		}
		tx2, err := eng2.BeginTx(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx2.Rollback() }()
		straddleExec(t, tx2, `MATCH (n {id: 1}) REMOVE n:L`)
		if err := straddleRun(eng2, straddleUniqueDDL); err != nil {
			t.Fatalf("CREATE CONSTRAINT: %v", err)
		}
		peer, err := eng2.BeginTx(ctx)
		if err != nil {
			t.Fatal(err)
		}
		straddleExec(t, peer, `CREATE (:L {s: 'x'})`)
		if err := txExecErr(tx2, `CREATE (:L {s: 'x'})`); !errors.Is(err, exec.ErrConstraintViolation) {
			t.Fatalf("taking 'x', reserved by a live peer, returned %v, want ErrConstraintViolation", err)
		}
		_ = peer.Rollback()
	})
}
