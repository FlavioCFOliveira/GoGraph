package cypher

// constraint_straddle_audit_test.go — the straddler attacks of the third ACID
// audit of rmp #2936, ported from the auditor's probe module (P1, P2, P4, P8)
// and generalised into the interleaving family H1 names.
//
// Layer: short.
//
// H1: a straddler's writes predate every constraint, so they never stamped the
// per-node constraint slot, and a peer that began after the registration could
// write the other half of the invariant — in a different substore — without
// colliding: UNIQUE then held two nodes with one value, NOT NULL a null. The fix
// stamps every touched node at the straddler's commit, before it validates.
//
// H2: a reservation taken in a constraint's value-set survived a DROP and
// re-CREATE, or a DROP whose durable commit failed and was rewound, although the
// new value-set was seeded without it; the commit-time validation then skipped
// the value as already reserved. The fix ties each reservation to the value-set
// generation it was taken in.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// straddleAssertUniqueExact asserts, for every candidate value, that at most one
// :L node carries it, that a second node with a held value is refused, and that
// a value no node holds is accepted — no duplicate and no phantom reservation.
func straddleAssertUniqueExact(t *testing.T, eng *Engine, values ...string) {
	t.Helper()
	for _, v := range values {
		n := straddleNodes(t, eng, v)
		if n > 1 {
			t.Errorf("s = %q: %d :L nodes under a live UNIQUE constraint", v, n)
			continue
		}
		err := straddleRun(eng, fmt.Sprintf(`CREATE (:L {s: '%s'})`, v))
		switch {
		case n == 1 && !errors.Is(err, exec.ErrConstraintViolation):
			t.Errorf("s = %q is held by a node, yet a second node with it returned %v", v, err)
		case n == 0 && err != nil:
			t.Errorf("s = %q is held by no node, yet a node with it was refused (%v): a phantom reservation", v, err)
		}
	}
}

// straddleAssertNoNull asserts that no :L node lacks s.
func straddleAssertNoNull(t *testing.T, eng *Engine) {
	t.Helper()
	if n := commitStateCount(t, eng, `MATCH (n:L) WHERE n.s IS NULL RETURN count(n) AS c`); n != 0 {
		t.Errorf("%d :L nodes lack s under a live NOT NULL constraint", n)
	}
}

// txExecErr runs q inside tx and returns its verdict.
func txExecErr(tx *ExplicitTx, q string) error {
	res, err := tx.ExecAny(q, nil)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	err = res.Err()
	_ = res.Close()
	return err
}

// TestConstraintStraddle_AuditP1_LabelGainVsPeerPropertySet is the auditor's P1.
func TestConstraintStraddle_AuditP1_LabelGainVsPeerPropertySet(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		ctx := context.Background()
		eng, _, _ := straddleEngine(t, walBacked)
		if err := straddleRun(eng, `CREATE ({id: 1, s: 'v'})`); err != nil {
			t.Fatal(err)
		}
		t1, _ := eng.BeginTx(ctx)
		straddleExec(t, t1, `MATCH (n {id: 1}) SET n:L`)
		if err := straddleRun(eng, "CREATE CONSTRAINT us FOR (n:L) REQUIRE n.s IS UNIQUE"); err != nil {
			t.Fatal(err)
		}
		t2, _ := eng.BeginTx(ctx)
		_ = txExecErr(t2, `MATCH (n {id: 1}) SET n.s = 'w'`)
		_ = t2.Commit()
		_ = t1.Commit()
		straddleAssertUniqueExact(t, eng, "v", "w")
	})
}

// TestConstraintStraddle_AuditP2_LabelGainVsPeerPropertyRemove is the auditor's P2.
func TestConstraintStraddle_AuditP2_LabelGainVsPeerPropertyRemove(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		ctx := context.Background()
		eng, _, _ := straddleEngine(t, walBacked)
		if err := straddleRun(eng, `CREATE ({id: 1, s: 'x'})`); err != nil {
			t.Fatal(err)
		}
		t1, _ := eng.BeginTx(ctx)
		straddleExec(t, t1, `MATCH (n {id: 1}) SET n:L`)
		if err := straddleRun(eng, "CREATE CONSTRAINT nn FOR (n:L) REQUIRE n.s IS NOT NULL"); err != nil {
			t.Fatal(err)
		}
		t2, _ := eng.BeginTx(ctx)
		_ = txExecErr(t2, `MATCH (n {id: 1}) REMOVE n.s`)
		_ = t2.Commit()
		_ = t1.Commit()
		straddleAssertNoNull(t, eng)
	})
}

// TestConstraintStraddle_AuditP4_ReservationAcrossDropAndRecreate is the
// auditor's P4, in both of its shapes: a DROP followed by a re-CREATE, and a DROP
// whose durable commit fails and is rewound — the seam fails it without
// poisoning the WAL, so the transaction can still commit afterwards.
//
// Each shape runs twice: with the transaction begun after the first CREATE (the
// auditor's order) and before it. In the second the reservation is taken under a
// constraint registered after the transaction began — the only kind it records
// — so it is the order in which a reservation from the old value-set exists at
// all and must not be counted in the new one.
func TestConstraintStraddle_AuditP4_ReservationAcrossDropAndRecreate(t *testing.T) {
	for _, shape := range []string{"drop-recreate", "drop-rewind"} {
		for _, beginFirst := range []bool{false, true} {
			name := shape + "/begin-after-create"
			if beginFirst {
				name = shape + "/begin-before-create"
			}
			t.Run(name, func(t *testing.T) {
				straddleWirings(t, func(t *testing.T, walBacked bool) {
					ctx := context.Background()
					eng, _, _ := straddleEngine(t, walBacked)
					const create = "CREATE CONSTRAINT us FOR (n:L) REQUIRE n.s IS UNIQUE"
					var t1 *ExplicitTx
					if beginFirst {
						t1, _ = eng.BeginTx(ctx)
					}
					if err := straddleRun(eng, create); err != nil {
						t.Fatal(err)
					}
					if !beginFirst {
						t1, _ = eng.BeginTx(ctx)
					}
					straddleExec(t, t1, `CREATE (:L {s: 'v'})`)
					if shape == "drop-recreate" {
						if err := straddleRun(eng, "DROP CONSTRAINT us"); err != nil {
							t.Fatal(err)
						}
						if err := straddleRun(eng, create); err != nil {
							t.Fatal(err)
						}
					} else {
						injected := errors.New("test: the DROP's durable commit failed")
						eng.dropCommitErrForTest = func() error { return injected }
						err := straddleRun(eng, "DROP CONSTRAINT us")
						eng.dropCommitErrForTest = nil
						if !errors.Is(err, injected) {
							t.Fatalf("fixture: DROP returned %v, want the injected failure", err)
						}
						if !eng.constraintAlreadyRegistered(exec.ConstraintUnique, "L", "s") {
							t.Fatal("fixture: the rewind did not restore the constraint")
						}
					}
					if err := t1.Commit(); err != nil {
						t.Fatalf("the transaction's unique value was refused: %v", err)
					}
					straddleAssertUniqueExact(t, eng, "v")
				})
			})
		}
	}
}

// TestConstraintStraddle_AuditP8_ValidatedStraddlerSurvivesReopen is the
// auditor's P8: a straddler validated at commit is replayed from the WAL, and the
// reopened engine enforces the constraint over it.
func TestConstraintStraddle_AuditP8_ValidatedStraddlerSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	eng, dir, wr := straddleEngine(t, true)
	t1, _ := eng.BeginTx(ctx)
	straddleExec(t, t1, `CREATE (:L {s: 'v'})`)
	if err := straddleRun(eng, "CREATE CONSTRAINT us FOR (n:L) REQUIRE n.s IS UNIQUE"); err != nil {
		t.Fatal(err)
	}
	if err := t1.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := wr.Close(); err != nil {
		t.Fatal(err)
	}
	opts := txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
	rec, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{Codec: opts.Codec, WeightCodec: opts.WeightCodec})
	if err != nil {
		t.Fatal(err)
	}
	wr2, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wr2.Close() })
	reopened := NewEngineWithStoreAndRecovery(txn.NewStoreWithOptions[string, float64](rec.Graph, wr2, opts), rec)
	straddleAssertUniqueExact(t, reopened, "v")
}

// TestConstraintStraddle_InterleavingFamily is H1's interleaving family. A
// straddler T either gains :L on a committed node (label-gain) or changes the
// value of a committed :L node (value-change), before or after the constraint
// is registered; a peer P that begins after the registration changes the
// property, removes it, removes the label, or deletes the node, before or after
// T's write, committing before or after T. Whatever commits, the constraint must
// hold exactly afterwards.
func TestConstraintStraddle_InterleavingFamily(t *testing.T) {
	type order struct {
		name string
		// steps, in order: "T" = the straddler's write, "D" = the DDL, "P" = the
		// peer's write, "cP"/"cT" = the commits. The peer begins right before its
		// write, always after the DDL.
		steps []string
	}
	orders := []order{
		{"T-D-P-cP-cT", []string{"T", "D", "P", "cP", "cT"}},
		{"T-D-P-cT-cP", []string{"T", "D", "P", "cT", "cP"}},
		{"D-P-cP-T-cT", []string{"D", "P", "cP", "T", "cT"}},
		{"D-P-T-cP-cT", []string{"D", "P", "T", "cP", "cT"}},
		{"D-P-T-cT-cP", []string{"D", "P", "T", "cT", "cP"}},
	}
	peers := map[string]string{
		"set":       `MATCH (n {id: 1}) SET n.s = 'w'`,
		"remove":    `MATCH (n {id: 1}) REMOVE n.s`,
		"unlabel":   `MATCH (n {id: 1}) REMOVE n:L`,
		"delete":    `MATCH (n {id: 1}) DETACH DELETE n`,
		"setOther":  `MATCH (n {id: 1}) SET n.s = 'x'`,
		"relabelOn": `MATCH (n {id: 1}) SET n:L`,
	}
	straddlers := map[string]struct{ seed, write string }{
		"label-gain":   {`CREATE ({id: 1, s: 'v'})`, `MATCH (n {id: 1}) SET n:L`},
		"value-change": {`CREATE (:L {id: 1, s: 'v'})`, `MATCH (n {id: 1}) SET n.s = 'u'`},
	}
	kinds := map[string]string{
		"unique":  "CREATE CONSTRAINT c FOR (n:L) REQUIRE n.s IS UNIQUE",
		"notnull": "CREATE CONSTRAINT c FOR (n:L) REQUIRE n.s IS NOT NULL",
	}
	for kind, ddl := range kinds {
		for sname, st := range straddlers {
			for pname, pq := range peers {
				for _, o := range orders {
					kind, ddl, st, pq, o := kind, ddl, st, pq, o
					name := fmt.Sprintf("%s/%s/%s/%s", kind, sname, pname, o.name)
					t.Run(name, func(t *testing.T) {
						straddleWirings(t, func(t *testing.T, walBacked bool) {
							ctx := context.Background()
							eng, _, _ := straddleEngine(t, walBacked)
							if err := straddleRun(eng, st.seed); err != nil {
								t.Fatal(err)
							}
							tx, err := eng.BeginTx(ctx)
							if err != nil {
								t.Fatal(err)
							}
							var peer *ExplicitTx
							for _, step := range o.steps {
								switch step {
								case "T":
									_ = txExecErr(tx, st.write)
								case "D":
									if err := straddleRun(eng, ddl); err != nil {
										// Refused by the build's live validation: a
										// legitimate outcome, and nothing is enforced.
										_ = tx.Rollback()
										return
									}
								case "P":
									peer, _ = eng.BeginTx(ctx)
									_ = txExecErr(peer, pq)
								case "cP":
									if peer.Commit() != nil {
										_ = peer.Rollback() // a poisoned transaction keeps its writes until rolled back
									}
								case "cT":
									if tx.Commit() != nil {
										_ = tx.Rollback()
									}
								}
							}
							if kind == "unique" {
								straddleAssertUniqueExact(t, eng, "u", "v", "w", "x")
							} else {
								straddleAssertNoNull(t, eng)
							}
						})
					})
				}
			}
		}
	}
}
