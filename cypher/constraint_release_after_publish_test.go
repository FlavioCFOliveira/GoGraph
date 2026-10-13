package cypher

// constraint_release_after_publish_test.go — a UNIQUE value a committing
// transaction vacates is released only after that transaction is visible
// (rmp #3128).
//
// The release used to be applied inside the commit, before the publication. A
// peer could reserve the freed value and publish first, so a snapshot taken
// between the two publications held the value on two nodes under a live UNIQUE
// constraint. The test runs the peer at that exact point, through the
// commit-decision seam, and asserts the peer is refused instead.
//
// Layer: short.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

func TestConstraintUnique_ReleaseWaitsForPublication(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		ctx := context.Background()
		eng, _, _ := straddleEngine(t, walBacked)
		for _, q := range []string{
			`CREATE (:P {id: 1, s: 'v'})`,
			`CREATE CONSTRAINT c FOR (n:P) REQUIRE n.s IS UNIQUE`,
		} {
			if err := straddleRun(eng, q); err != nil {
				t.Fatal(err)
			}
		}
		const holders = `MATCH (n:P) WHERE n.s = 'v' RETURN count(n) AS c`
		tx, err := eng.BeginTx(ctx)
		if err != nil {
			t.Fatal(err)
		}
		straddleExec(t, tx, `MATCH (n:P {id: 1}) REMOVE n.s`)

		// Between the commit decision and the publication: the removal is decided
		// but not visible, so node 1 still holds 'v' for every reader.
		var once sync.Once
		var peerErr error
		var seen int64 = -1
		eng.commitDecidedHookForTest = func() {
			once.Do(func() {
				peerErr = straddleRun(eng, `CREATE (:P {id: 2, s: 'v'})`)
				seen = commitStateCount(t, eng, holders)
			})
		}
		err = tx.Commit()
		eng.commitDecidedHookForTest = nil
		if err != nil {
			t.Fatalf("COMMIT: %v", err)
		}
		if seen == -1 {
			t.Fatal("the commit never reached its decision seam")
		}
		if seen > 1 {
			t.Fatalf("%d nodes visibly held 'v' under a live UNIQUE constraint", seen)
		}
		if !errors.Is(peerErr, exec.ErrConstraintViolation) {
			t.Fatalf("peer CREATE of 'v' before the release was visible: want a constraint violation, got %v", peerErr)
		}
		// After the publication the value is free: no reservation leaked.
		if err := straddleRun(eng, `CREATE (:P {id: 3, s: 'v'})`); err != nil {
			t.Fatalf("CREATE of 'v' after the release: %v", err)
		}
		if c := commitStateCount(t, eng, holders); c != 1 {
			t.Fatalf("after the release: %d nodes hold 'v', want 1", c)
		}
	})
}

// TestConstraintUnique_RollbackReleasesNothingAndLeaksNothing: a rolled-back
// transaction frees no value it vacated, and keeps no value it reserved.
func TestConstraintUnique_RollbackReleasesNothingAndLeaksNothing(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		ctx := context.Background()
		eng, _, _ := straddleEngine(t, walBacked)
		for _, q := range []string{
			`CREATE (:P {id: 1, s: 'old'})`,
			`CREATE CONSTRAINT c FOR (n:P) REQUIRE n.s IS UNIQUE`,
		} {
			if err := straddleRun(eng, q); err != nil {
				t.Fatal(err)
			}
		}
		tx, err := eng.BeginTx(ctx)
		if err != nil {
			t.Fatal(err)
		}
		straddleExec(t, tx, `MATCH (n:P {id: 1}) SET n.s = 'new'`)
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if err := straddleRun(eng, `CREATE (:P {id: 2, s: 'new'})`); err != nil {
			t.Fatalf("'new' stayed reserved after the rollback: %v", err)
		}
		if err := straddleRun(eng, `CREATE (:P {id: 3, s: 'old'})`); !errors.Is(err, exec.ErrConstraintViolation) {
			t.Fatalf("'old' was released by a rolled-back transaction: want a constraint violation, got %v", err)
		}
	})
}
