package cypher

// constraint_ddl_dml_race_test.go — CREATE CONSTRAINT racing concurrent writes
// never admits a violation (rmp #3082).
//
// rmp #3082 lets a write of a key no constraint names skip the constraint
// registry's exclusive lock, deciding "unconstrained" from a lock-free snapshot
// of the catalogue. The hazard that decision could open is the DDL-vs-DML race:
// a write judged unconstrained just before a constraint on its key registers,
// committing a value that violates it. This test runs seeded rounds in which
// writers — autocommit and explicit transactions, writing the constrained key s
// and the unconstrained key z — race a loop of CREATE / DROP CONSTRAINT on s,
// and after every successful CREATE asserts the committed state satisfies the
// constraint. Both kinds are raced: UNIQUE (writers draw s from a small domain,
// so duplicates are frequent) and NOT NULL (writers REMOVE s).
//
// Layer: short. Race-clean. Both wirings; the WAL wiring injects the seeded
// fsync latency so commits keep a real window open.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
)

const (
	ddlDMLNodes   = 8
	ddlDMLWriters = 4
	ddlDMLRounds  = 12 // in-memory wiring; the WAL wiring runs ddlDMLRoundsWAL
	// ddlDMLRoundsWAL is smaller because every WAL commit pays the injected
	// fsync latency: 12 rounds x 3 seeds measured 25.4 s there, against 0.9 s
	// in memory.
	ddlDMLRoundsWAL = 4
	ddlDMLDomain    = 48 // s values: P(all 8 distinct) ≈ 0.55, so CREATE UNIQUE succeeds often
)

func TestConstraintDDLvsDML_NoViolationAdmitted(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		seeds, rounds := []uint64{1, 2, 3}, ddlDMLRounds
		if walBacked {
			seeds, rounds = []uint64{1}, ddlDMLRoundsWAL
		}
		for _, seed := range seeds {
			t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
				runDDLvsDML(t, walBacked, seed, rounds)
			})
		}
	})
}

func runDDLvsDML(t *testing.T, walBacked bool, seed uint64, rounds int) {
	ctx := context.Background()
	eng, _, _ := straddleEngine(t, walBacked)
	for i := 0; i < ddlDMLNodes; i++ {
		if err := straddleRun(eng, fmt.Sprintf(`CREATE (:P {id: %d, s: 's%d', z: 0})`, i, i)); err != nil {
			t.Fatal(err)
		}
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	var commits atomic.Int64
	for w := 0; w < ddlDMLWriters; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(seed, uint64(w))) //nolint:gosec // G404: math/rand/v2 PCG seeded from the test's own parameter — this test asserts a reproducible sequence, which a CSPRNG would destroy.
			for !stop.Load() {
				id := rng.IntN(ddlDMLNodes)
				id2 := rng.IntN(ddlDMLNodes)
				v := fmt.Sprintf("s%d", rng.IntN(ddlDMLDomain))
				var q string
				switch rng.IntN(4) {
				case 0:
					q = fmt.Sprintf(`MATCH (n:P {id: %d}) SET n.s = '%s', n.z = n.z + 1`, id, v)
				case 1:
					q = fmt.Sprintf(`MATCH (n:P {id: %d}) REMOVE n.s SET n.z = n.z + 1`, id)
				default:
					q = fmt.Sprintf(`MATCH (n:P {id: %d}) SET n.z = n.z + 1`, id)
				}
				if rng.IntN(2) == 0 {
					if straddleRun(eng, q) == nil {
						commits.Add(1)
					}
					continue
				}
				// Explicit transaction: the constrained write and an
				// unconstrained one in separate statements, so a CREATE
				// CONSTRAINT can land between them.
				tx, err := eng.BeginTx(ctx)
				if err != nil {
					continue
				}
				ok := txExecQuiet(tx, q) == nil &&
					txExecQuiet(tx, fmt.Sprintf(`MATCH (n:P {id: %d}) SET n.z = n.z + 1`, id2)) == nil
				if !ok {
					_ = tx.Rollback()
					continue
				}
				if tx.Commit() == nil {
					commits.Add(1)
				} else {
					_ = tx.Rollback()
				}
			}
		}(w)
	}

	created := 0
	for round := 0; round < rounds; round++ {
		unique := round%2 == 0
		ddl := `CREATE CONSTRAINT c FOR (n:P) REQUIRE n.s IS NOT NULL`
		if unique {
			ddl = `CREATE CONSTRAINT c FOR (n:P) REQUIRE n.s IS UNIQUE`
		}
		// The CREATE fails while the committed state already violates it;
		// retry a bounded number of times while the writers move it on.
		var err error
		for attempt := 0; attempt < 200; attempt++ {
			if err = straddleRun(eng, ddl); err == nil {
				break
			}
		}
		if err != nil {
			continue
		}
		created++
		// Let the writers run against the live constraint, checking the
		// committed state repeatedly while they do.
		for check := 0; check < 20; check++ {
			if unique {
				if c := ddlDMLCount(t, eng, `MATCH (n:P) WHERE n.s IS NOT NULL WITH n.s AS s, count(*) AS k WHERE k > 1 RETURN count(*) AS c`); c != 0 {
					stop.Store(true)
					wg.Wait()
					t.Fatalf("round %d: %d duplicated s values committed under a live UNIQUE constraint", round, c)
				}
			} else if c := ddlDMLCount(t, eng, `MATCH (n:P) WHERE n.s IS NULL RETURN count(n) AS c`); c != 0 {
				stop.Store(true)
				wg.Wait()
				t.Fatalf("round %d: %d :P nodes without s committed under a live NOT NULL constraint", round, c)
			}
		}
		if err := straddleRun(eng, `DROP CONSTRAINT c`); err != nil {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("DROP CONSTRAINT: %v", err)
		}
		// Restore s on every node so the next NOT NULL CREATE can succeed.
		for i := 0; i < ddlDMLNodes; i++ {
			_ = straddleRun(eng, fmt.Sprintf(`MATCH (n:P {id: %d}) WHERE n.s IS NULL SET n.s = 'r%d'`, i, i))
		}
	}
	stop.Store(true)
	wg.Wait()
	// The evidence was produced: constraints were live while writers committed.
	t.Logf("constraints created %d of %d rounds, writer commits %d", created, rounds, commits.Load())
	if created == 0 {
		t.Fatal("no CREATE CONSTRAINT succeeded: the race was never exercised")
	}
	if commits.Load() == 0 {
		t.Fatal("no writer committed: the race was never exercised")
	}
}

// txExecQuiet runs q inside tx and returns its verdict.
func txExecQuiet(tx *ExplicitTx, q string) error {
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

// ddlDMLCount returns the integer column c of q's single row.
func ddlDMLCount(t *testing.T, eng *Engine, q string) int64 {
	t.Helper()
	return commitStateCount(t, eng, q)
}
