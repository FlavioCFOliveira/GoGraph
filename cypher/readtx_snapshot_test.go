package cypher_test

// readtx_snapshot_test.go — an explicit read transaction observes ONE instant
// for all of its statements (rmp #2307, sprint 334).
//
// Before this, [cypher.Engine.BeginReadTx] acquired nothing and routed every
// Exec back through [cypher.Engine.Run], which opened a FRESH snapshot per
// statement. The handle therefore delivered READ-COMMITTED across statements: a
// commit landing between two statements of an open read transaction became
// visible mid-transaction. That made an explicit read transaction strictly
// WEAKER than a single autocommit statement, which has always had one instant
// for its whole duration — exactly backwards.
//
// TestReadTx_SnapshotIsolationAcrossStatements is the gate on that, and it was
// verified to FAIL against the read-committed build: with the per-statement
// snapshot it reports "second statement saw 2 nodes, want 1" — it observes the
// commit that landed between the two statements.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// autocommit runs one statement outside any explicit transaction and drains it,
// failing the test on any error. It is the "someone else commits" half of every
// isolation assertion below.
func autocommit(t *testing.T, eng *cypher.Engine, query string) {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("autocommit %q: %v", query, err)
	}
	for res.Next() { //nolint:revive // intentional full drain
	}
	if err := res.Err(); err != nil {
		t.Fatalf("autocommit %q: %v", query, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("autocommit %q close: %v", query, err)
	}
}

// countPInTx runs a count inside the given handle and returns the single scalar
// row it produces.
func countPInTx(t *testing.T, tx *cypher.ExplicitTx) int64 {
	t.Helper()
	res, err := tx.Exec("MATCH (p:P) RETURN count(p) AS n", nil)
	if err != nil {
		t.Fatalf("Exec count: %v", err)
	}
	defer func() {
		if err := res.Close(); err != nil {
			t.Fatalf("close count result: %v", err)
		}
	}()
	if !res.Next() {
		t.Fatalf("count returned no row (err=%v)", res.Err())
	}
	n, ok := res.ValueAt(0).(expr.IntegerValue)
	if !ok {
		t.Fatalf("count returned %T, want IntegerValue", res.ValueAt(0))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("count drain: %v", err)
	}
	return int64(n)
}

// TestReadTx_SnapshotIsolationAcrossStatements is acceptance criterion 1 of
// rmp #2307: every statement of an explicit read transaction observes the same
// instant, so a commit that lands between two of them is invisible to the
// second.
func TestReadTx_SnapshotIsolationAcrossStatements(t *testing.T) {
	t.Parallel()
	eng, _ := storelessEngineWithGraph(t)
	autocommit(t, eng, "CREATE (:P {n: 1})")

	rtx, err := eng.BeginReadTx(context.Background())
	if err != nil {
		t.Fatalf("BeginReadTx: %v", err)
	}
	defer func() { _ = rtx.Rollback() }()

	if got := countPInTx(t, rtx); got != 1 {
		t.Fatalf("first statement saw %d nodes, want 1", got)
	}

	// A whole other transaction commits, and acknowledges the commit, while the
	// read transaction is open. Under snapshot isolation the read transaction's
	// instant predates it.
	autocommit(t, eng, "CREATE (:P {n: 2})")

	if got := countPInTx(t, rtx); got != 1 {
		t.Fatalf("second statement saw %d nodes, want 1: the read transaction "+
			"observed a commit that landed after it began, which is read-committed, not snapshot isolation", got)
	}
	// A third statement must agree with the first two — the instant is fixed,
	// not merely lagging by one.
	if got := countPInTx(t, rtx); got != 1 {
		t.Fatalf("third statement saw %d nodes, want 1", got)
	}

	if err := rtx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// After the transaction ends, a fresh read sees everything committed.
	after, err := eng.BeginReadTx(context.Background())
	if err != nil {
		t.Fatalf("BeginReadTx after: %v", err)
	}
	if got := countPInTx(t, after); got != 2 {
		t.Fatalf("a read transaction opened AFTER the commit saw %d nodes, want 2: "+
			"the snapshot is stuck rather than per-transaction", got)
	}
	if err := after.Commit(); err != nil {
		t.Fatalf("Commit after: %v", err)
	}
}

// TestReadTx_PinsReclamationHorizon is acceptance criterion 3 of rmp #2307: an
// open read transaction holds a horizon slot for its whole lifetime, MVCCStats
// says so and names it as the reason reclamation is held back, and the slot is
// returned when the transaction finishes.
//
// A transaction-lifetime snapshot is a real cost, not a free upgrade: versions
// the transaction can still reach cannot be reclaimed while it is open. This
// test exists so that cost is observable rather than mysterious.
func TestReadTx_PinsReclamationHorizon(t *testing.T) {
	t.Parallel()
	eng, g := storelessEngineWithGraph(t)
	autocommit(t, eng, "CREATE (:P {n: 0})")

	base := g.MVCCStats()
	rtx, err := eng.BeginReadTx(context.Background())
	if err != nil {
		t.Fatalf("BeginReadTx: %v", err)
	}

	during := g.MVCCStats()
	if during.ActiveReaders != base.ActiveReaders+1 {
		t.Fatalf("ActiveReaders %d while a read transaction is open, want %d: "+
			"the handle did not register with the horizon",
			during.ActiveReaders, base.ActiveReaders+1)
	}
	if during.UnregisteredReaders != 0 {
		t.Fatalf("UnregisteredReaders %d: the reader could not get a slot, so nothing is reclaimed",
			during.UnregisteredReaders)
	}

	// Advance the clock underneath the open transaction. Its watermark must stay
	// behind, which is precisely what "it pins the horizon" means and what
	// OldestReaderAge is for.
	for i := 1; i <= 5; i++ {
		autocommit(t, eng, "CREATE (:P)")
	}
	held := g.MVCCStats()
	if held.OldestReaderAge() == 0 {
		t.Fatalf("OldestReaderAge is 0 with an open read transaction and %d commits since it began "+
			"(watermark %d, now %d): the horizon is not being pinned",
			5, held.Watermark, held.Now)
	}

	if err := rtx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	after := g.MVCCStats()
	if after.ActiveReaders != base.ActiveReaders {
		t.Fatalf("ActiveReaders %d after Commit, want %d: the horizon slot was not returned",
			after.ActiveReaders, base.ActiveReaders)
	}
}

// TestReadTx_ReleasesHorizonSlotOnEveryExitPath is acceptance criterion 4 of
// rmp #2307. A slot that is never returned pins the watermark for the life of
// the process, and one returned twice corrupts it for every other reader, so
// the release must be exactly-once on every way out of the handle.
func TestReadTx_ReleasesHorizonSlotOnEveryExitPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// finish drives the handle to its end by one route.
		finish func(t *testing.T, tx *cypher.ExplicitTx)
	}{
		{
			name:   "Commit",
			finish: func(t *testing.T, tx *cypher.ExplicitTx) { mustFinish(t, tx.Commit()) },
		},
		{
			name:   "Rollback",
			finish: func(t *testing.T, tx *cypher.ExplicitTx) { mustFinish(t, tx.Rollback()) },
		},
		{
			// The second call must be rejected rather than release a second time.
			name: "Commit then Commit",
			finish: func(t *testing.T, tx *cypher.ExplicitTx) {
				mustFinish(t, tx.Commit())
				if err := tx.Commit(); !errors.Is(err, cypher.ErrTxFinished) {
					t.Fatalf("second Commit returned %v, want ErrTxFinished", err)
				}
			},
		},
		{
			name: "Commit then Rollback",
			finish: func(t *testing.T, tx *cypher.ExplicitTx) {
				mustFinish(t, tx.Commit())
				if err := tx.Rollback(); !errors.Is(err, cypher.ErrTxFinished) {
					t.Fatalf("Rollback after Commit returned %v, want ErrTxFinished", err)
				}
			},
		},
		{
			// A statement that errors leaves the handle open; the caller still
			// owns finishing it, and that must still return the slot.
			name: "rejected write then Rollback",
			finish: func(t *testing.T, tx *cypher.ExplicitTx) {
				if _, err := tx.Exec("CREATE (:X)", nil); !errors.Is(err, cypher.ErrWriteInReadOnlyTx) {
					t.Fatalf("write in read-only tx returned %v, want ErrWriteInReadOnlyTx", err)
				}
				mustFinish(t, tx.Rollback())
			},
		},
		{
			// A cancelled context makes further statements fail; finishing must
			// still work, and must still return the slot.
			name: "cancelled context then Rollback",
			finish: func(t *testing.T, tx *cypher.ExplicitTx) {
				mustFinish(t, tx.Rollback())
			},
		},
		{
			// The panic path. A panic raised INSIDE a read statement never
			// reaches here: Engine.runRead's own recover boundary
			// (recoverQueryPanic) converts it to an error and leaves the handle
			// open, so it degenerates to the "statement errored" case above.
			// What is left is a panic in the CALLER between Exec and finishing,
			// where the documented contract — finish the handle with exactly one
			// Commit or Rollback — is discharged by a deferred Rollback. That
			// deferred call is the only thing standing between an unwinding
			// caller and a horizon slot pinned for the life of the process, so
			// it is what this case asserts.
			name: "panic in the caller, unwound through a deferred Rollback",
			finish: func(t *testing.T, tx *cypher.ExplicitTx) {
				func() {
					defer func() {
						if r := recover(); r == nil {
							t.Error("expected the injected panic to unwind")
						}
					}()
					defer func() { mustFinish(t, tx.Rollback()) }()
					if _, err := tx.Exec("MATCH (p:P) RETURN p", nil); err != nil {
						t.Errorf("Exec: %v", err)
					}
					panic("caller panics mid-transaction")
				}()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng, g := storelessEngineWithGraph(t)
			autocommit(t, eng, "CREATE (:P)")

			base := g.MVCCStats()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tx, err := eng.BeginReadTx(ctx)
			if err != nil {
				t.Fatalf("BeginReadTx: %v", err)
			}
			if got := g.MVCCStats().ActiveReaders; got != base.ActiveReaders+1 {
				t.Fatalf("ActiveReaders %d after BeginReadTx, want %d", got, base.ActiveReaders+1)
			}
			if tc.name == "cancelled context then Rollback" {
				cancel()
				if _, err := tx.Exec("MATCH (p:P) RETURN p", nil); err == nil {
					t.Fatal("Exec on a cancelled context returned no error")
				}
			}

			tc.finish(t, tx)

			if got := g.MVCCStats().ActiveReaders; got != base.ActiveReaders {
				t.Fatalf("ActiveReaders %d after %s, want %d: the horizon slot was not returned exactly once",
					got, tc.name, base.ActiveReaders)
			}
			// The watermark must be free to advance again. With no reader
			// registered, Horizon.Oldest falls back to the clock's current
			// instant, so Watermark == Now and OldestReaderAge is 0; a leaked
			// slot would hold the watermark at the finished transaction's start
			// timestamp and make the age non-zero.
			autocommit(t, eng, "CREATE (:P)")
			if s := g.MVCCStats(); s.OldestReaderAge() != 0 {
				t.Fatalf("OldestReaderAge %d after %s with no active readers (watermark %d, now %d): "+
					"a horizon slot is still held", s.OldestReaderAge(), tc.name, s.Watermark, s.Now)
			}
		})
	}
}

func mustFinish(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("finishing the read transaction: %v", err)
	}
}

// TestReadTx_SnapshotSurvivesDisarmedVersioning covers the wiring's one
// degenerate input: with versioning disarmed, [lpg.Graph.BeginRead] returns a
// nil snapshot, which legitimately means "read the current value". The handle
// must still work and must still be releasable — the nil is a valid view, not a
// missing one, which is why the pinned view is carried in a wrapper rather than
// signalled by a nil snapshot.
func TestReadTx_SnapshotSurvivesDisarmedVersioning(t *testing.T) {
	t.Parallel()
	g := newDisarmedGraph(t)
	eng := cypher.NewEngine(g)
	autocommit(t, eng, "CREATE (:P)")

	tx, err := eng.BeginReadTx(context.Background())
	if err != nil {
		t.Fatalf("BeginReadTx: %v", err)
	}
	if got := countPInTx(t, tx); got != 1 {
		t.Fatalf("saw %d nodes, want 1", got)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := g.MVCCStats().ActiveReaders; got != 0 {
		t.Fatalf("ActiveReaders %d with versioning disarmed, want 0", got)
	}
}

// newDisarmedGraph builds a graph with the versioning substrate off.
func newDisarmedGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	_, g := storelessEngineWithGraph(t)
	g.DisableMVCC()
	return g
}
