package cypher

// constraint_straddle_test.go — a transaction open across CREATE CONSTRAINT is
// validated against the new constraint at its commit (rmp #2936; see
// exec.ConstraintRegistry.ValidateStraddler), and a DROP CONSTRAINT whose durable
// commit fails does not restore a constraint over a duplicate committed in
// between.
//
// Layer: short.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/synclatency"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// straddleEngine returns an empty engine on either wiring, and the directory of
// its WAL ("" for the in-memory wiring).
func straddleEngine(t *testing.T, walBacked bool) (*Engine, string, *wal.Writer) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	if !walBacked {
		return NewEngine(g), "", nil
	}
	dir := t.TempDir()
	wr, err := wal.OpenWithSyncLatency(filepath.Join(dir, "wal"), synclatency.ForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wr.Close() })
	st := txn.NewStoreWithOptions[string, float64](g, wr, txn.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	return NewEngineWithStore(st), dir, wr
}

// straddleRun runs q autocommit and returns its verdict.
func straddleRun(eng *Engine, q string) error {
	res, err := eng.RunAny(context.Background(), q, nil)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	err = res.Err()
	_ = res.Close()
	return err
}

// straddleExec runs q inside tx.
func straddleExec(t *testing.T, tx *ExplicitTx, q string) {
	t.Helper()
	res, err := tx.ExecAny(q, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	for res.Next() {
	}
	_ = res.Close()
}

// straddleNodes counts the :L nodes whose s equals v, by a scan.
func straddleNodes(t *testing.T, eng *Engine, v string) int64 {
	return commitStateCount(t, eng, fmt.Sprintf(`MATCH (n:L) WHERE n.s + '' = '%s' RETURN count(n) AS c`, v))
}

func straddleWirings(t *testing.T, fn func(t *testing.T, walBacked bool)) {
	for _, walBacked := range []bool{false, true} {
		name := "mem"
		if walBacked {
			name = "wal"
		}
		t.Run(name, func(t *testing.T) { fn(t, walBacked) })
	}
}

// TestConstraintStraddle_UniqueValidValueCommitsAndIsReserved: a value written
// before the constraint existed commits — it is unique — and is then reserved, so
// a second node with it is refused. Before the fix the second node committed.
func TestConstraintStraddle_UniqueValidValueCommitsAndIsReserved(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		ctx := context.Background()
		eng, _, _ := straddleEngine(t, walBacked)
		tx, err := eng.BeginTx(ctx)
		if err != nil {
			t.Fatal(err)
		}
		straddleExec(t, tx, `CREATE (:L {s: 'v'})`)
		if err := straddleRun(eng, "CREATE CONSTRAINT us FOR (n:L) REQUIRE n.s IS UNIQUE"); err != nil {
			t.Fatalf("CREATE CONSTRAINT: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("the straddler with a unique value was refused: %v", err)
		}
		if err := straddleRun(eng, `CREATE (:L {s: 'v'})`); !errors.Is(err, exec.ErrConstraintViolation) {
			t.Fatalf("a second node with s = 'v' returned %v, want ErrConstraintViolation: the straddler's "+
				"value was never reserved", err)
		}
		if n := straddleNodes(t, eng, "v"); n != 1 {
			t.Fatalf("%d nodes with s = 'v' under a live UNIQUE constraint, want 1", n)
		}
	})
}

// TestConstraintStraddle_NotNullRefusesAtCommit: a node written, without the
// property, after CREATE CONSTRAINT's validation scan and before its registration
// is refused at commit, and leaves no trace. Before the fix it committed.
func TestConstraintStraddle_NotNullRefusesAtCommit(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		ctx := context.Background()
		eng, dir, wr := straddleEngine(t, walBacked)
		tx, err := eng.BeginTx(ctx)
		if err != nil {
			t.Fatal(err)
		}
		eng.constraintValidatedHookForTest = func() { straddleExec(t, tx, `CREATE (:L {t: 'no-s'})`) }
		err = straddleRun(eng, "CREATE CONSTRAINT nn FOR (n:L) REQUIRE n.s IS NOT NULL")
		eng.constraintValidatedHookForTest = nil
		if err != nil {
			t.Fatalf("CREATE CONSTRAINT: %v", err)
		}
		cerr := tx.Commit()
		if !errors.Is(cerr, exec.ErrConstraintViolation) || !strings.Contains(fmt.Sprint(cerr), `"nn"`) {
			t.Fatalf("Commit of a node lacking s under a NOT NULL constraint returned %v, want "+
				"ErrConstraintViolation naming the constraint", cerr)
		}
		if c := commitStateCount(t, eng, `MATCH (n:L) RETURN count(n) AS c`); c != 0 {
			t.Fatalf("%d :L nodes after the refused commit, want 0", c)
		}
		if walBacked {
			straddleAssertReopened(t, wr, dir, `MATCH (n:L) RETURN count(n) AS c`, 0)
		}
	})
}

// straddleAssertReopened closes the WAL, recovers the directory and asserts the
// count q returns there.
func straddleAssertReopened(t *testing.T, wr *wal.Writer, dir, q string, want int64) {
	t.Helper()
	if err := wr.Close(); err != nil {
		t.Fatal(err)
	}
	rec, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if c := commitStateCount(t, NewEngine(rec.Graph), q); c != want {
		t.Fatalf("after reopening, %s = %d, want %d: the refused transaction reached the WAL", q, c, want)
	}
}

// TestConstraintStraddle_TwoStraddlersOneValue: two transactions write the same
// value after CREATE CONSTRAINT's validation scan, and commit at once. Exactly
// one commits; the other is refused and leaves no trace. Before the fix both
// committed.
func TestConstraintStraddle_TwoStraddlersOneValue(t *testing.T) {
	straddleWirings(t, func(t *testing.T, walBacked bool) {
		ctx := context.Background()
		eng, dir, wr := straddleEngine(t, walBacked)
		t1, err := eng.BeginTx(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t2, err := eng.BeginTx(ctx)
		if err != nil {
			t.Fatal(err)
		}
		eng.constraintValidatedHookForTest = func() {
			straddleExec(t, t1, `CREATE (:L {s: 'z', who: 1})`)
			straddleExec(t, t2, `CREATE (:L {s: 'z', who: 2})`)
		}
		err = straddleRun(eng, "CREATE CONSTRAINT us FOR (n:L) REQUIRE n.s IS UNIQUE")
		eng.constraintValidatedHookForTest = nil
		if err != nil {
			t.Fatalf("CREATE CONSTRAINT: %v", err)
		}
		errs := make(chan error, 2)
		start := make(chan struct{})
		for _, tx := range []*ExplicitTx{t1, t2} {
			go func(tx *ExplicitTx) { <-start; errs <- tx.Commit() }(tx)
		}
		close(start)
		var ok, refused int
		for i := 0; i < 2; i++ {
			switch err := <-errs; {
			case err == nil:
				ok++
			case errors.Is(err, exec.ErrConstraintViolation):
				refused++
			default:
				t.Fatalf("Commit: %v", err)
			}
		}
		if ok != 1 || refused != 1 {
			t.Fatalf("%d commits and %d refusals, want exactly one of each", ok, refused)
		}
		if n := straddleNodes(t, eng, "z"); n != 1 {
			t.Fatalf("%d nodes with s = 'z', want 1", n)
		}
		if seek, scan := commitStateSeekVsScan(t, eng, "z"); seek != scan {
			t.Fatalf("seek %d, scan %d: the refused transaction left an index entry", seek, scan)
		}
		if err := straddleRun(eng, `CREATE (:L {s: 'z'})`); !errors.Is(err, exec.ErrConstraintViolation) {
			t.Fatalf("a third node with s = 'z' returned %v, want ErrConstraintViolation", err)
		}
		if err := straddleRun(eng, `CREATE (:L {s: 'y'})`); err != nil {
			t.Fatalf("a fresh value was refused: %v — the refused transaction left a reservation", err)
		}
		if walBacked {
			straddleAssertReopened(t, wr, dir, `MATCH (n:L) WHERE n.s + '' = 'z' RETURN count(n) AS c`, 1)
		}
	})
}

// TestConstraintStraddle_DropRewindKeepsNoDuplicate: a DROP CONSTRAINT whose
// durable commit fails restores the constraint. An explicit transaction that
// wrote a duplicate while the constraint was unregistered must not commit into
// the gap and survive the restoration. Before the fix it did: the restored UNIQUE
// constraint held two nodes with one value.
func TestConstraintStraddle_DropRewindKeepsNoDuplicate(t *testing.T) {
	e, fs, _ := newRewindEngine(t)
	ctx := context.Background()
	if err := rewindStmt(t, e, rewindCreateDDL); err != nil {
		t.Fatalf("%q: %v", rewindCreateDDL, err)
	}
	tx, err := e.BeginTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	committed := make(chan error, 1)
	e.constraintDroppedHookForTest = func() {
		straddleExec(t, tx, fmt.Sprintf(`CREATE (:Person {name: '%s'})`, rewindReal))
		go func() { committed <- tx.Commit() }()
		select {
		case <-committed:
			committed <- nil // it committed inside the gap; re-publish for the reader below
		case <-time.After(300 * time.Millisecond):
		}
		fs.failNextSync.Store(true) // the DROP's own durable commit fails next
	}
	derr := rewindStmt(t, e, rewindDropDDL)
	e.constraintDroppedHookForTest = nil
	if derr == nil {
		t.Fatal("fixture: the DROP succeeded although its fsync was armed to fail")
	}
	<-committed
	if !e.constraintAlreadyRegistered(exec.ConstraintUnique, "Person", "name") {
		t.Fatal("fixture: the failed DROP did not restore the constraint")
	}
	if c := commitStateCount(t, e, fmt.Sprintf(`MATCH (n:Person) WHERE n.name + '' = '%s' RETURN count(n) AS c`, rewindReal)); c != 1 {
		t.Fatalf("%d :Person nodes named %q under the restored UNIQUE constraint, want 1", c, rewindReal)
	}
}
