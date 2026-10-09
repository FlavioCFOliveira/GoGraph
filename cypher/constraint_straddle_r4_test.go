package cypher

// constraint_straddle_r4_test.go — the fourth ACID audit's probes of rmp #2936,
// ported as regression tests (R4-1: a peer that committed before the constraint
// existed; R4-2: a value moving between a transaction's own nodes; R4-3: an
// inverse deleting from a re-created value-set), plus the auditor's replay and
// stamp probes. Each asserts the constraint's invariant only while the
// constraint is live: a CREATE CONSTRAINT refused by its validation — which reads
// uncommitted writes and is conservative by design — is a legitimate outcome.
//
// Layer: short.

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/internal/synclatency"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

const r4Unique = "CREATE CONSTRAINT us FOR (n:L) REQUIRE n.s IS UNIQUE"

func TestConstraintStraddle_R4A_PeerCommittedBeforeTheConstraint(t *testing.T) {
	for _, side := range []string{"label-side", "property-side"} {
		t.Run(side, func(t *testing.T) {
			straddleWirings(t, func(t *testing.T, walBacked bool) {
				ctx := context.Background()
				eng, _, _ := straddleEngine(t, walBacked)
				_ = straddleRun(eng, `CREATE (:P {id: 1, s: 'a'})`)
				tx, _ := eng.BeginTx(ctx)
				if side == "label-side" {
					straddleExec(t, tx, `MATCH (n:P {id: 1}) SET n:L`)
					_ = straddleRun(eng, `MATCH (n:P {id: 1}) SET n.s = 'b'`)
				} else {
					straddleExec(t, tx, `MATCH (n:P {id: 1}) SET n.s = 'v'`)
					_ = straddleRun(eng, `MATCH (n:P {id: 1}) SET n:L`)
				}
				if err := straddleRun(eng, r4Unique); err != nil {
					_ = tx.Rollback()
					return
				}
				if err := tx.Commit(); err != nil {
					_ = tx.Rollback()
				}
				straddleAssertUniqueExact(t, eng, "a", "b", "v")
			})
		})
	}
}

func TestConstraintStraddle_R4A_NotNullPeerCommittedBeforeTheConstraint(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		ctx := context.Background()
		eng, _, _ := straddleEngine(t, walBacked)
		_ = straddleRun(eng, `CREATE (:P {id: 1, s: 'a'})`)
		tx, _ := eng.BeginTx(ctx)
		straddleExec(t, tx, `MATCH (n:P {id: 1}) SET n:L`)
		_ = straddleRun(eng, `MATCH (n:P {id: 1}) REMOVE n.s`)
		if err := straddleRun(eng, "CREATE CONSTRAINT nn FOR (n:L) REQUIRE n.s IS NOT NULL"); err != nil {
			_ = tx.Rollback()
			return
		}
		if err := tx.Commit(); err != nil {
			_ = tx.Rollback()
		}
		straddleAssertNoNull(t, eng)
	})
}

func TestConstraintStraddle_R4B_ValueMovesBetweenOwnNodes(t *testing.T) {
	moves := map[string][]string{
		"set":                {`MATCH (n:L {id: 1}) SET n.s = 'w'`, `CREATE (:L {id: 2, s: 'v'})`},
		"label-loss":         {`MATCH (n:L {id: 1}) REMOVE n:L`, `CREATE (:L {id: 2, s: 'v'})`},
		"delete-then-create": {`MATCH (n:L {id: 1}) DELETE n`, `CREATE (:L {id: 2, s: 'v'})`},
	}
	for name, stmts := range moves {
		t.Run(name, func(t *testing.T) {
			straddleWirings(t, func(t *testing.T, walBacked bool) {
				ctx := context.Background()
				eng, _, _ := straddleEngine(t, walBacked)
				_ = straddleRun(eng, `CREATE (:L {id: 1, s: 'v'})`)
				tx, _ := eng.BeginTx(ctx)
				for _, q := range stmts {
					straddleExec(t, tx, q)
				}
				if err := straddleRun(eng, r4Unique); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatalf("a valid move was refused: %v", err)
				}
				straddleAssertUniqueExact(t, eng, "v", "w")
			})
		})
	}
}

func TestConstraintStraddle_R4B_DeleteBeforeAnyIndexLeavesNoPhantom(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		ctx := context.Background()
		eng, _, _ := straddleEngine(t, walBacked)
		_ = straddleRun(eng, `CREATE (:L {id: 1, s: 'v'})`)
		tx, _ := eng.BeginTx(ctx)
		straddleExec(t, tx, `MATCH (n:L {id: 1}) DELETE n`)
		if err := straddleRun(eng, r4Unique); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		straddleAssertUniqueExact(t, eng, "v")
	})
}

// TestConstraintStraddle_R4C_InverseAfterTheValueSetIsRebuilt: a transaction
// reserves 'v' under the constraint, the constraint's value-set is rebuilt — by a
// DROP and re-CREATE, or by a DROP whose durable commit fails and is rewound —
// a peer takes 'v' in the new set, and the transaction then ends. Whether it
// rolls back or is refused at commit, its old reservation's inverse must not
// delete the peer's value from the new set.
func TestConstraintStraddle_R4C_InverseAfterTheValueSetIsRebuilt(t *testing.T) {
	for _, rebuild := range []string{"drop-create", "drop-rewound"} {
		for _, end := range []string{"rollback", "commit"} {
			t.Run(rebuild+"/"+end, func(t *testing.T) {
				straddleWirings(t, func(t *testing.T, walBacked bool) {
					ctx := context.Background()
					eng, _, _ := straddleEngine(t, walBacked)
					if err := straddleRun(eng, r4Unique); err != nil {
						t.Fatal(err)
					}
					tx, _ := eng.BeginTx(ctx)
					straddleExec(t, tx, `CREATE (:L {s: 'v'})`)
					if rebuild == "drop-create" {
						_ = straddleRun(eng, "DROP CONSTRAINT us")
						_ = straddleRun(eng, r4Unique)
					} else {
						eng.dropCommitErrForTest = func() error { return errors.New("test: injected") }
						_ = straddleRun(eng, "DROP CONSTRAINT us")
						eng.dropCommitErrForTest = nil
					}
					if err := straddleRun(eng, `CREATE (:L {s: 'v'})`); err != nil {
						t.Fatalf("the peer could not take 'v' in the rebuilt value-set: %v", err)
					}
					if end == "rollback" {
						_ = tx.Rollback()
					} else if err := tx.Commit(); !errors.Is(err, exec.ErrConstraintViolation) {
						t.Fatalf("the transaction's duplicate 'v' committed (%v)", err)
					}
					straddleAssertUniqueExact(t, eng, "v")
				})
			})
		}
	}
}

func TestConstraintStraddle_R4C_ReserveThenDropCreateCommits(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		ctx := context.Background()
		eng, _, _ := straddleEngine(t, walBacked)
		_ = straddleRun(eng, r4Unique)
		tx, _ := eng.BeginTx(ctx)
		straddleExec(t, tx, `CREATE (:L {s: 'v'})`)
		_ = straddleRun(eng, "DROP CONSTRAINT us")
		_ = straddleRun(eng, r4Unique)
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		straddleAssertUniqueExact(t, eng, "v")
	})
}

// TestConstraintStraddle_P5_TwoStraddlersOneValueBeforeTheDDL is the auditor's
// P5 with the oracle applied only while the constraint is live: two open
// transactions holding the same value make the DDL's validation refuse.
func TestConstraintStraddle_P5_TwoStraddlersOneValueBeforeTheDDL(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		ctx := context.Background()
		eng, _, _ := straddleEngine(t, walBacked)
		t1, _ := eng.BeginTx(ctx)
		t2, _ := eng.BeginTx(ctx)
		straddleExec(t, t1, `CREATE (:L {s: 'v'})`)
		straddleExec(t, t2, `CREATE (:L {s: 'v'})`)
		ddlErr := straddleRun(eng, r4Unique)
		for _, tx := range []*ExplicitTx{t1, t2} {
			if tx.Commit() != nil {
				_ = tx.Rollback()
			}
		}
		if ddlErr == nil {
			straddleAssertUniqueExact(t, eng, "v")
		}
	})
}

func TestConstraintStraddle_R4D_RefusedStraddlerIsNotReplayed(t *testing.T) {
	for _, mode := range []string{"unique", "stamp"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			eng, dir, wr := straddleEngine(t, true)
			_ = straddleRun(eng, `CREATE ({id: 1, s: 'v'})`)
			tx, _ := eng.BeginTx(ctx)
			straddleExec(t, tx, `CREATE (:L {s: 'z', tag: 'straddler'})`)
			straddleExec(t, tx, `MATCH (n {id: 1}) SET n:L`)
			if err := straddleRun(eng, r4Unique); err != nil {
				t.Fatal(err)
			}
			if mode == "unique" {
				_ = straddleRun(eng, `CREATE (:L {s: 'z'})`)
			} else {
				_ = straddleRun(eng, `MATCH (n {id: 1}) SET n.s = 'w'`)
			}
			if err := tx.Commit(); err == nil {
				t.Fatal("the straddler committed, want a refusal")
			}
			_ = tx.Rollback()
			if err := wr.Close(); err != nil {
				t.Fatal(err)
			}
			opts := txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
			rec, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{Codec: opts.Codec, WeightCodec: opts.WeightCodec})
			if err != nil {
				t.Fatal(err)
			}
			wr2, err := wal.OpenWithSyncLatency(filepath.Join(dir, "wal"), synclatency.ForTest(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = wr2.Close() })
			reopened := NewEngineWithStoreAndRecovery(txn.NewStoreWithOptions[string, float64](rec.Graph, wr2, opts), rec)
			if n := commitStateCount(t, reopened, `MATCH (n {tag: 'straddler'}) RETURN count(n) AS c`); n != 0 {
				t.Fatalf("the refused straddler's node was replayed: %d", n)
			}
			if n := commitStateCount(t, reopened, `MATCH (n:L {id: 1}) RETURN count(n) AS c`); n != 0 {
				t.Fatalf("the refused straddler's label was replayed: %d", n)
			}
		})
	}
}

func TestConstraintStraddle_R4E_StampOfARefusedStraddlerIsReleased(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		ctx := context.Background()
		eng, _, _ := straddleEngine(t, walBacked)
		_ = straddleRun(eng, `CREATE ({id: 1, s: 'v'}), ({id: 2, s: 'q'})`)
		tx, _ := eng.BeginTx(ctx)
		straddleExec(t, tx, `MATCH (n {id: 1}) SET n:L`)
		straddleExec(t, tx, `MATCH (n {id: 2}) SET n:L`)
		_ = straddleRun(eng, r4Unique)
		_ = straddleRun(eng, `MATCH (n {id: 1}) SET n.s = 'w'`)
		if err := tx.Commit(); err == nil {
			t.Fatal("expected a refusal")
		}
		_ = tx.Rollback()
		for i := 0; i < 3; i++ {
			tx2, _ := eng.BeginTx(ctx)
			if err := txExecErr(tx2, `MATCH (n {id: 2}) SET n:L, n.s = 'r'`); err != nil {
				t.Fatalf("a write after the refusal failed: %v", err)
			}
			if err := tx2.Commit(); err != nil {
				t.Fatalf("a commit after the refusal was blocked by a leftover stamp: %v", err)
			}
		}
	})
}

func TestConstraintStraddle_R4E_TwoStraddlersStampingInOppositeOrders(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		iterations := 50
		if walBacked {
			iterations = 10
		}
		for it := 0; it < iterations; it++ {
			ctx := context.Background()
			eng, _, _ := straddleEngine(t, walBacked)
			_ = straddleRun(eng, `CREATE ({id: 1, s: 'a'}), ({id: 2, s: 'b'})`)
			t1, _ := eng.BeginTx(ctx)
			t2, _ := eng.BeginTx(ctx)
			straddleExec(t, t1, `MATCH (n {id: 1}) SET n:L`)
			straddleExec(t, t1, `MATCH (n {id: 2}) SET n.x = 1`)
			straddleExec(t, t2, `MATCH (n {id: 2}) SET n:L`)
			straddleExec(t, t2, `MATCH (n {id: 1}) SET n.y = 1`)
			_ = straddleRun(eng, r4Unique)
			var wg sync.WaitGroup
			wg.Add(2)
			for _, tx := range []*ExplicitTx{t1, t2} {
				go func(tx *ExplicitTx) {
					defer wg.Done()
					if tx.Commit() != nil {
						_ = tx.Rollback()
					}
				}(tx)
			}
			wg.Wait() // a deadlock would hang here
			straddleAssertUniqueExact(t, eng, "a", "b")
		}
	})
}
