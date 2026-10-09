package cypher

// constraint_straddle_r5_test.go — the fifth ACID audit's probes of rmp #2936,
// ported as regression tests (R5-1). A straddler released a value judging from
// its snapshot, a peer that committed before the constraint existed had moved
// that value to a node the straddler never touched, and the straddler's write
// then took the value: the reservation inserted nothing, yet the commit
// validation treated it as the straddler's own — skipping the check, and
// releasing the value at commit when the write was given up.
//
// Layer: short.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// r5StaleSpend runs the R5-1 interleaving and returns the straddler's commit
// verdict: node 1 holds 'v' when T begins; a peer moves 'v' from node 1 to a
// new node 3 before the constraint exists; T, whose snapshot still shows node 1
// with 'v', releases node 1 by release and then runs each of writes.
func r5StaleSpend(t *testing.T, walBacked bool, release string, writes ...string) (*Engine, error) {
	t.Helper()
	ctx := context.Background()
	eng, _, _ := straddleEngine(t, walBacked)
	if err := straddleRun(eng, `CREATE (:L {id: 1, s: 'v'})`); err != nil {
		t.Fatal(err)
	}
	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := straddleRun(eng, `MATCH (n:L {id: 1}) SET n.s = 'x' CREATE (:L {id: 3, s: 'v'})`); err != nil {
		t.Fatalf("peer: %v", err)
	}
	if err := straddleRun(eng, r4Unique); err != nil {
		t.Fatalf("constraint: %v", err)
	}
	for _, q := range append([]string{release}, writes...) {
		if err := txExecErr(tx, q); err != nil {
			_ = tx.Rollback()
			return eng, err
		}
	}
	cerr := tx.Commit()
	if cerr != nil {
		_ = tx.Rollback()
	}
	return eng, cerr
}

// TestConstraintStraddle_R5A_StaleReleaseSpend is the auditor's probe 8: T's
// CREATE of 'v' must be refused, because node 3 holds it. Before the fix T
// committed and two :L nodes held 'v', on both wirings.
func TestConstraintStraddle_R5A_StaleReleaseSpend(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		eng, err := r5StaleSpend(t, walBacked, `MATCH (n:L {id: 1}) REMOVE n:L`, `CREATE (:L {id: 2, s: 'v'})`)
		if !errors.Is(err, exec.ErrConstraintViolation) {
			t.Errorf("straddler taking a value an untouched node holds: got %v, want a UNIQUE violation", err)
		}
		straddleAssertUniqueExact(t, eng, "v", "x")
	})
}

// TestConstraintStraddle_R5A_StaleReleaseSpendDelete is probe 8's second shape,
// the release made by DELETE. The deletion collides with the peer's write to
// node 1, so it was refused before the fix as well; it guards the life side of
// the same interleaving.
func TestConstraintStraddle_R5A_StaleReleaseSpendDelete(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		eng, _ := r5StaleSpend(t, walBacked, `MATCH (n:L {id: 1}) DELETE n`, `CREATE (:L {id: 2, s: 'v'})`)
		straddleAssertUniqueExact(t, eng, "v", "x")
	})
}

// TestConstraintStraddle_R5B_StaleReserveThenGiveUp is the auditor's probe 10:
// T takes 'v' without inserting it and deletes that node again, so T holds no
// 'v' and may commit — but its commit must not release 'v', which node 3 still
// holds. Before the fix the commit released it and a fresh CREATE of 'v' was
// accepted: two :L nodes with one UNIQUE value, on both wirings.
func TestConstraintStraddle_R5B_StaleReserveThenGiveUp(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		eng, err := r5StaleSpend(t, walBacked, `MATCH (n:L {id: 1}) REMOVE n:L`,
			`CREATE (:L {id: 2, s: 'v'})`, `MATCH (n:L {id: 2}) DELETE n`)
		if err != nil {
			t.Errorf("straddler that holds no contested value: commit returned %v", err)
		}
		straddleAssertUniqueExact(t, eng, "v", "x")
	})
}
