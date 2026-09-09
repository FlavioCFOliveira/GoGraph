package cypher

// constraint_rewind_snapshot_test.go — rmp #2799 gate: the DROP CONSTRAINT
// rewind rebuilds both UNIQUE enforcement structures from COMMITTED state, at
// one instant, and adds no exclusion to do it.
//
// # Reachability — the window is real, and reached by execution
//
// [Engine.rewindConstraintDrop] runs only after commitConstraintTx has failed,
// which for DROP CONSTRAINT means the WAL append or its fsync failed. These
// tests open that window for real rather than modelling it: a fault-injecting
// [wal.WALFile] (rewindWALFS, wired through [wal.OpenFS] — the same seam
// store/checkpoint's failed-DDL battery uses) fails exactly one fsync, so
// [txn.Tx.CommitWALOnly] returns [wal.ErrDurabilityFailed] and the rewind runs
// on the real path with real state.
//
// Measured on this build: the DROP returns the durability error, the constraint
// is STILL REGISTERED afterwards (that is the rewind's own signature — without it
// the constraint would have vanished in memory while remaining durable), and the
// UNIQUE backing index is present again.
//
// # The defect these tests pin
//
// The rewind rebuilt the backing index AND re-seeded the value-set from the LIVE
// property bag, which carries an explicit transaction's EAGER, UNCOMMITTED
// mutations. [ExplicitTx.Rollback] discards that transaction's
// [exec.IndexBuffer] WITHOUT inverting it — the buffer describes changes that
// were never fanned out — so whatever the rebuild wrote stayed there.
//
// This is rmp #2778's and rmp #2792's defect on the third path, which both of
// those tasks deliberately left. Measured on the pre-fix build, ONE GOROUTINE and
// NO CONCURRENCY, with the eager mutation open across a DROP CONSTRAINT whose
// fsync failed:
//
//	setProperty  ghost 'seed-1-ghost' = 1 entry (want 0), real 'seed-7' = 0 (want 1)
//	             seek 'seed-1-ghost' -> [k7]  FABRICATED   seek 'seed-7' -> []  LOST
//	createNode   ghost = 1 entry (want 0)                  real = 1
//	removeLabel  ghost = 0                                 real = 0 (want 1)  LOST
//	deleteNode   ghost = 0                                 real = 0 (want 1)  LOST
//
// After the fix all four arms report ghost = 0 and real = 1, and every seek
// agrees with a label scan.
//
// # Why a snapshot here is not the trade rmp #2792 refused to make
//
// rmp #2792's backfill sits inside the visibility barrier under an EXCLUSIVE
// hold, so nothing can commit between its snapshot and the registration. This
// rewind held no barrier, and a snapshot taken outside one cannot see a value
// committed after it was taken — which would drop that value from the value-set
// and let a genuine duplicate through. That trade is worse than the defect, which
// is why extending #2792 by one line here was refused.
//
// The window is closed on both counts instead of traded, and both halves are
// asserted below:
//
//   - the rewind now runs INSIDE [lpg.Graph.ApplyAtomically], so the snapshot,
//     the backfill and the registration are one instant, and
//   - the rewind is reachable ONLY on a WAL that can no longer accept a frame.
//     TestRewindConstraintDrop_NothingCanCommitAcrossTheRewind measures it: an
//     explicit transaction held OPEN across the whole failed DROP cannot commit
//     afterwards, and its node never becomes visible.
//
// # What the oracle is
//
// A label scan over the SAME predicate on a SECOND engine that carries no
// constraint and has run the SAME transaction and rollback against the SAME
// poisoned WAL. The assertion is set equality, so a fabricated row and a lost row
// are both caught and neither can mask the other. Enforcement verdicts are read
// as [exec.ErrConstraintViolation] or its absence: on a poisoned WAL no write can
// be made durable, but the UNIQUE check runs BEFORE the WAL, so the verdict the
// value-set produces is still exactly observable — a refusal is the constraint's,
// and a durability error is the constraint declining to refuse.
//
// Layer: short. goleak-clean (engines, graphs and WAL writers are local and
// closed by t.Cleanup).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// rewindDropDDL is the statement whose failure opens the window under test.
const rewindDropDDL = `DROP CONSTRAINT c_name`

// rewindCreateDDL registers the constraint the DROP then fails to remove. The
// seed's names are distinct, so it is valid over the committed data.
const rewindCreateDDL = `CREATE CONSTRAINT c_name FOR (n:Person) REQUIRE n.name IS UNIQUE`

// rewindGhost is a value NO committed node holds. The rolled-back transaction
// writes it; a seek for it must return nothing and the constraint must not refuse
// a write of it.
const rewindGhost = "seed-1-ghost"

// rewindReal is the committed value of the node every mutation below targets
// (tag k7): the entry the rebuild should write and, before the fix, did not.
const rewindReal = "seed-7"

// rewindLate is committed AFTER the constraint is created and BEFORE the failing
// DROP. It is the control for the LOSS a snapshot could have introduced: a
// value-set rebuilt from a stale instant would not hold it, and a duplicate of it
// would then be accepted under an active UNIQUE constraint.
const rewindLate = "late-1"

// errRewindFsync is the fault a rewindWALFile returns for the one armed fsync.
var errRewindFsync = errors.New("constraint_rewind_snapshot_test: injected fsync failure")

// rewindWALFS delegates every WAL filesystem operation to the real OS but wraps
// each opened file so a single fsync can be made to fail on demand. Its methods
// are all exported and its file type is the exported [wal.WALFile], so it
// structurally satisfies the wal package's unexported walFS interface and can be
// passed to [wal.OpenFS] — the same seam store/checkpoint's failed-DDL battery
// and the deterministic-simulation harness use.
type rewindWALFS struct {
	failNextSync atomic.Bool // when set, the NEXT rewindWALFile.Sync fails once
}

func (fs *rewindWALFS) OpenFile(path string, flag int) (wal.WALFile, error) {
	f, err := os.OpenFile(path, flag, 0o600) //nolint:gosec // test-controlled temp path
	if err != nil {
		return nil, err
	}
	return &rewindWALFile{File: f, fs: fs}, nil
}

func (fs *rewindWALFS) Rename(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }

func (fs *rewindWALFS) Remove(path string) error { return os.Remove(path) }

func (fs *rewindWALFS) ParentDirSync(childPath string) error {
	d, err := os.Open(filepath.Dir(childPath))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// rewindWALFile wraps an *os.File and overrides Sync so a single, armed fsync
// fails. It is deliberately NOT an *os.File, so wal's dataSync type assertion
// falls through to this Sync on every platform and the injected fault fires on
// the commit path.
type rewindWALFile struct {
	*os.File
	fs *rewindWALFS
}

func (f *rewindWALFile) Sync() error {
	if f.fs.failNextSync.CompareAndSwap(true, false) {
		return errRewindFsync
	}
	return f.File.Sync()
}

// newRewindEngine builds a WAL-backed engine over the shared seed whose next
// fsync can be made to fail.
func newRewindEngine(tb testing.TB) (*Engine, *rewindWALFS, *wal.Writer) {
	tb.Helper()
	dir := tb.TempDir()
	fs := &rewindWALFS{}
	w, err := wal.OpenFS(fs, filepath.Join(dir, "wal"))
	if err != nil {
		tb.Fatalf("wal.OpenFS: %v", err)
	}
	tb.Cleanup(func() { _ = w.Close() }) // returns the sticky poison error once armed
	store := txn.NewStoreWithOptions[string, float64](backfillSnapshotSeed(tb), w,
		txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()})
	return NewEngineWithStore(store), fs, w
}

// rewindStmt runs a statement through the engine and returns the terminal error
// the caller saw — nil means it was applied and made durable, and on the arms
// below a non-nil error is either the UNIQUE refusal or the WAL poison.
func rewindStmt(tb testing.TB, e *Engine, stmt string) error {
	tb.Helper()
	res, err := e.RunAny(context.Background(), stmt, nil)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	verdict := res.Err()
	_ = res.Close()
	return verdict
}

// rewindMutation is one shape of eager, uncommitted write the rebuild must not
// observe. The four shapes cover each thing the scan resolves — the property
// VALUE, node EXISTENCE in both directions, and the LABEL — and are the same four
// rmp #2778 and rmp #2792 use, so the three gates are comparable arm for arm.
//
// setProperty and createNode FABRICATE an entry; setProperty, removeLabel and
// deleteNode LOSE one. Both directions are needed, which is why the query-level
// oracle is set equality rather than "no unexpected rows".
type rewindMutation struct {
	name string
	stmt string
}

func rewindMutations() []rewindMutation {
	return []rewindMutation{
		{"setProperty", `MATCH (n:Person) WHERE n.tag = 'k7' SET n.name = '` + rewindGhost + `'`},
		{"createNode", `CREATE (m:Person {tag: 'ghostnode', name: '` + rewindGhost + `'})`},
		{"removeLabel", `MATCH (n:Person) WHERE n.tag = 'k7' REMOVE n:Person`},
		{"deleteNode", `MATCH (n:Person) WHERE n.tag = 'k7' DETACH DELETE n`},
	}
}

// newRewindEngineAfterFailedDrop builds a WAL-backed engine, registers the
// constraint, commits rewindLate, opens an explicit transaction, runs stmt inside
// it, arms one fsync failure and runs DROP CONSTRAINT — which fails at the WAL
// and therefore runs the rewind — while that transaction is still OPEN, and only
// then rolls the transaction back.
//
// withConstraint=false builds the ORACLE: the same seed, the same committed late
// value, the same rolled-back transaction, and the same poisoned WAL, but no
// constraint and no DROP, so its answers come from a label scan. The poison is
// applied at the point the DROP would have failed, so the rollback runs against
// the same store state in both engines and the two differ in the constraint
// alone.
//
// The whole sequence is on ONE goroutine and needs no synchronisation: the DROP
// runs to completion between the eager mutation and the rollback by program
// order, so the window under test cannot fail to open. That is the point — this
// defect needs no concurrency at all.
func newRewindEngineAfterFailedDrop(tb testing.TB, stmt string, withConstraint bool) *Engine {
	tb.Helper()
	e, fs, w := newRewindEngine(tb)
	ctx := context.Background()

	if withConstraint {
		if err := rewindStmt(tb, e, rewindCreateDDL); err != nil {
			tb.Fatalf("%q: %v", rewindCreateDDL, err)
		}
	}
	// Committed AFTER the constraint, BEFORE the failing DROP: the value a
	// stale-instant rebuild would lose.
	late := `CREATE (:Person {tag: 'late', name: '` + rewindLate + `'})`
	if err := rewindStmt(tb, e, late); err != nil {
		tb.Fatalf("%q: %v", late, err)
	}

	tx, err := e.BeginTx(ctx)
	if err != nil {
		tb.Fatalf("BeginTx: %v", err)
	}
	sres, serr := tx.Exec(stmt, nil)
	if serr != nil {
		tb.Fatalf("eager %q: %v", stmt, serr)
	}
	for sres.Next() {
	}
	if derr := sres.Err(); derr != nil {
		tb.Fatalf("eager drain %q: %v", stmt, derr)
	}
	_ = sres.Close()

	fs.failNextSync.Store(true)
	if withConstraint {
		derr := rewindStmt(tb, e, rewindDropDDL)
		if derr == nil {
			tb.Fatalf("%q unexpectedly SUCCEEDED with an armed fsync failure — the rewind "+
				"window never opened, so every assertion below would be vacuous", rewindDropDDL)
		}
		if !errors.Is(derr, wal.ErrDurabilityFailed) {
			tb.Fatalf("%q failed with %v, want a %v — the rewind runs only on the WAL "+
				"failure path and this is not it", rewindDropDDL, derr, wal.ErrDurabilityFailed)
		}
	} else {
		// Same poison, applied where the DROP would have failed, so the oracle's
		// rollback runs against the same store state.
		if serr := w.Sync(); serr == nil {
			tb.Fatalf("oracle: the armed fsync failure did not poison the WAL")
		}
	}

	if err := tx.Rollback(); err != nil {
		tb.Fatalf("Rollback: %v", err)
	}
	return e
}

// rewindBackingCardinality reports how many nodes the UNIQUE backing index holds
// under value.
func rewindBackingCardinality(tb testing.TB, e *Engine, value string) uint64 {
	tb.Helper()
	return hashIndexCardinality(tb, e, exec.UniqueIndexName("Person", "name"), value)
}

// TestRewindConstraintDrop_RunsAfterAFailedWALSync is the REACHABILITY gate: it
// records, by execution, that the window every other test in this file depends on
// is real — a DROP CONSTRAINT whose fsync fails leaves the constraint registered
// (the rewind's signature) with its backing index rebuilt.
func TestRewindConstraintDrop_RunsAfterAFailedWALSync(t *testing.T) {
	t.Parallel()
	e, fs, w := newRewindEngine(t)
	if err := rewindStmt(t, e, rewindCreateDDL); err != nil {
		t.Fatalf("%q: %v", rewindCreateDDL, err)
	}
	if !e.constraintReg.HasUnique("Person", "name") {
		t.Fatalf("the constraint was not registered by %q", rewindCreateDDL)
	}

	fs.failNextSync.Store(true)
	derr := rewindStmt(t, e, rewindDropDDL)
	if derr == nil {
		t.Fatalf("%q succeeded although its fsync was armed to fail", rewindDropDDL)
	}
	if !errors.Is(derr, wal.ErrDurabilityFailed) {
		t.Fatalf("%q failed with %v, want %v", rewindDropDDL, derr, wal.ErrDurabilityFailed)
	}
	if !e.constraintReg.HasUnique("Person", "name") {
		t.Fatalf("after a DROP CONSTRAINT that failed at the WAL the constraint is GONE from " +
			"the registry: the rewind did not run, so the constraint has vanished in memory " +
			"while remaining durable and the next reopen would resurrect it")
	}
	if _, gerr := e.g.IndexManager().GetIndex(exec.UniqueIndexName("Person", "name")); gerr != nil {
		t.Fatalf("after the rewind the UNIQUE backing index is missing: %v", gerr)
	}
	if w.Poisoned() == nil {
		t.Fatalf("the WAL is not poisoned after an injected fsync failure — the reachability " +
			"argument recorded on rewindConstraintDrop rests on this")
	}
}

// TestRewindConstraintDrop_NothingCanCommitAcrossTheRewind is the BARRIER gate's
// reachability half: the rewind is reached only on a WAL that can no longer
// accept a frame, so the LOSS a snapshot could have traded a fabricated entry for
// — a value committed after the snapshot was taken — has no way to occur.
//
// An explicit transaction is held OPEN across the whole failed DROP, so its
// commit is exactly the "concurrently committed value" the hazard names. It must
// fail, and its node must never become visible.
func TestRewindConstraintDrop_NothingCanCommitAcrossTheRewind(t *testing.T) {
	t.Parallel()
	e, fs, _ := newRewindEngine(t)
	ctx := context.Background()
	if err := rewindStmt(t, e, rewindCreateDDL); err != nil {
		t.Fatalf("%q: %v", rewindCreateDDL, err)
	}
	tx, err := e.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	sres, serr := tx.Exec(`CREATE (:Person {tag: 'straddle', name: 'straddle-1'})`, nil)
	if serr != nil {
		t.Fatalf("straddling write: %v", serr)
	}
	for sres.Next() {
	}
	if derr := sres.Err(); derr != nil {
		t.Fatalf("straddling drain: %v", derr)
	}
	_ = sres.Close()

	fs.failNextSync.Store(true)
	if derr := rewindStmt(t, e, rewindDropDDL); derr == nil {
		t.Fatalf("%q succeeded although its fsync was armed to fail", rewindDropDDL)
	}

	cerr := tx.Commit()
	if cerr == nil {
		t.Fatalf("a transaction held open across the rewind COMMITTED. The snapshot the rewind " +
			"reads is then not the final committed state, and the value this transaction " +
			"committed is lost from the value-set — which is the trade rmp #2799 exists to " +
			"refuse. Re-derive the reachability argument on rewindConstraintDrop before " +
			"trusting the snapshot")
	}
	if !errors.Is(cerr, wal.ErrDurabilityFailed) {
		t.Fatalf("the straddling commit failed with %v, want %v — the reachability argument "+
			"names the WAL poison as the mechanism", cerr, wal.ErrDurabilityFailed)
	}
	rows := backfillSnapshotRows(t, e, `MATCH (n:Person) WHERE n.tag = 'straddle' RETURN n.tag`)
	if len(rows) != 0 {
		t.Fatalf("the straddling transaction's node is visible (%v) after its commit failed", rows)
	}
}

// TestRewindConstraintDrop_BackingIndexMatchesCommittedState is the CONTENT gate:
// the rebuilt UNIQUE backing index must hold entries for EXACTLY the committed
// state — none for a value only an uncommitted transaction wrote, and one for the
// value the graph actually holds.
//
// It is asserted at content level as well as at query level because the two catch
// different instances of the same defect: on the createNode shape the rolled-back
// node loses its label, so [exec.NodeByIndexSeek]'s label residual filters the
// fabricated entry and no wrong ROW reaches the caller — yet an index that
// permanently retains an entry for a value no transaction ever committed is a
// defect whether or not today's residual masks it.
//
// Sensitivity: the real-value assertion is the ghost assertion's control. Both
// read the same accessor on the same index in the same call shape, so a zero for
// the ghost cannot be an accessor that never sees anything while the same
// accessor reports one for rewindReal. A COMMITTED write cannot serve as the
// control here — the rewind is only reachable on a WAL that refuses every
// further commit.
func TestRewindConstraintDrop_BackingIndexMatchesCommittedState(t *testing.T) {
	t.Parallel()
	for _, mut := range rewindMutations() {
		t.Run(mut.name, func(t *testing.T) {
			t.Parallel()
			e := newRewindEngineAfterFailedDrop(t, mut.stmt, true)

			if got := rewindBackingCardinality(t, e, rewindGhost); got != 0 {
				t.Errorf("after %q was rolled back across the rewind, the rebuilt UNIQUE backing "+
					"index holds %d entr(ies) for the GHOST value %q — the rebuild indexed a "+
					"value no transaction ever committed, and the rollback does not invert it "+
					"because the change was never fanned out", mut.stmt, got, rewindGhost)
			}
			if got := rewindBackingCardinality(t, e, rewindReal); got != 1 {
				t.Errorf("after %q was rolled back across the rewind, the rebuilt UNIQUE backing "+
					"index holds %d entr(ies) for the COMMITTED value %q of the mutated node, "+
					"want exactly 1 — the rebuild read the transaction's uncommitted state "+
					"instead of the committed one, so the real entry was never written",
					mut.stmt, got, rewindReal)
			}
			if got := rewindBackingCardinality(t, e, rewindLate); got != 1 {
				t.Errorf("the rebuilt UNIQUE backing index holds %d entr(ies) for %q, want "+
					"exactly 1. That value was COMMITTED after the constraint and before the "+
					"failing DROP, so a rebuild that read a stale instant LOST it", got, rewindLate)
			}
		})
	}
}

// TestRewindConstraintDrop_BackingIndexAnswersWhatTheGraphHolds is the
// QUERY-level gate: a seek served by the rebuilt UNIQUE backing index must return
// exactly what a label scan over the same predicate returns.
//
// The oracle is a second engine with no constraint, carrying the same seed, the
// same committed late value, the same rolled-back transaction and the same
// poisoned WAL. Non-vacuity is asserted on the REAL value, where the index is
// chosen in both builds — asserting it on the ghost value would be asserting the
// defect, since a fabricated entry is part of what makes the planner choose the
// index for an absent value.
func TestRewindConstraintDrop_BackingIndexAnswersWhatTheGraphHolds(t *testing.T) {
	t.Parallel()
	for _, mut := range rewindMutations() {
		t.Run(mut.name, func(t *testing.T) {
			t.Parallel()
			indexed := newRewindEngineAfterFailedDrop(t, mut.stmt, true)
			oracle := newRewindEngineAfterFailedDrop(t, mut.stmt, false)

			for _, value := range []string{rewindGhost, rewindReal, rewindLate} {
				query := `MATCH (n:Person) WHERE n.name = '` + value + `' RETURN n.tag`
				if plan := planOf(t, oracle, query, nil); strings.Contains(plan, "NodeByIndexSeek") {
					t.Fatalf("%s: the oracle is not an oracle — no constraint was created on it, "+
						"yet its plan uses a seek:\n%s", query, plan)
				}
				want := backfillSnapshotRows(t, oracle, query)
				got := backfillSnapshotRows(t, indexed, query)
				if !sameStringSet(want, got) {
					t.Fatalf("%s\nafter %q was rolled back across the rewind:\n"+
						"  constrained engine = %d rows %v\n"+
						"  label scan         = %d rows %v\n"+
						"  constrained plan   = %s\n"+
						"the two must be identical: extra rows are FABRICATED (the graph does "+
						"not hold them) and missing rows are LOST",
						query, mut.stmt, len(got), summariseRows(got), len(want), summariseRows(want),
						planOf(t, indexed, query, nil))
				}
			}

			realProbe := `MATCH (n:Person) WHERE n.name = '` + rewindReal + `' RETURN n.tag`
			if plan := planOf(t, indexed, realProbe, nil); !strings.Contains(plan, "NodeByIndexSeek") {
				t.Fatalf("%s: the comparison above was vacuous — the constrained engine's plan "+
					"does not use the rebuilt backing index after %q was rolled back across the "+
					"rewind:\n%s", realProbe, mut.stmt, plan)
			}
		})
	}
}

// TestRewindConstraintDrop_UniqueEnforcementMatchesCommittedState is the
// VALUE-SET gate, asserted in BOTH directions through the only oracle that does
// not ask the structure under test to report on itself: the enforcement verdict.
//
// A refusal carries [exec.ErrConstraintViolation] and is produced by the UNIQUE
// check, which runs BEFORE the WAL. On a poisoned WAL a write the constraint does
// NOT refuse still fails, with the durability error — so the two verdicts stay
// exactly distinguishable, and "not refused" is asserted as "did not carry
// ErrConstraintViolation", never as "succeeded".
//
// The probed values are the cells the defect moves: the mutated node's committed
// value, the value committed between the CREATE and the failing DROP, two
// untouched committed values, and two values no committed node holds.
func TestRewindConstraintDrop_UniqueEnforcementMatchesCommittedState(t *testing.T) {
	t.Parallel()
	for _, mut := range rewindMutations() {
		t.Run(mut.name, func(t *testing.T) {
			t.Parallel()
			// removeLabel and deleteNode leave k7 committed and labelled after the
			// rollback, so rewindReal is a committed duplicate on every arm.
			refuse := []string{rewindReal, rewindLate, "seed-3", "seed-11"}
			accept := []string{rewindGhost, "no-node-holds-this"}

			e := newRewindEngineAfterFailedDrop(t, mut.stmt, true)
			for _, v := range refuse {
				stmt := `CREATE (:Person {tag: 'probe', name: '` + v + `'})`
				err := rewindStmt(t, e, stmt)
				if !errors.Is(err, exec.ErrConstraintViolation) {
					t.Errorf("after %q was rolled back across the rewind, a write of the "+
						"COMMITTED value %q was NOT refused by the UNIQUE constraint (err=%v). "+
						"The re-seeded value-set does not hold a value the committed graph does, "+
						"so a genuine duplicate passes enforcement", mut.stmt, v, err)
				}
			}
			for _, v := range accept {
				stmt := `CREATE (:Person {tag: 'probe', name: '` + v + `'})`
				err := rewindStmt(t, e, stmt)
				if errors.Is(err, exec.ErrConstraintViolation) {
					t.Errorf("after %q was rolled back across the rewind, a write of %q was "+
						"REFUSED as a duplicate although no committed node holds it (err=%v). "+
						"The re-seeded value-set holds a phantom, and it refuses a legitimate "+
						"write for as long as the engine runs", mut.stmt, v, err)
				}
			}
		})
	}
}

// TestRewindConstraintDrop_AddsNoExclusion pins the structural half of the fix:
// the rewind takes the visibility barrier, and NOT the schema gate on an explicit
// transaction's behalf. rmp #2738 measured that making an explicit transaction
// take the schema gate closes a three-way cycle with the store's quiesce and
// hangs; the documented order stays schemaGate -> writer admission -> visMu.
//
// An explicit transaction executes statements on a second goroutine for the whole
// duration of the failing DROP. If the DDL had started excluding it, either the
// DDL or the transaction would stop making progress; the test fails on the
// timeout rather than hanging the suite.
func TestRewindConstraintDrop_AddsNoExclusion(t *testing.T) {
	t.Parallel()
	e, fs, _ := newRewindEngine(t)
	ctx := context.Background()
	if err := rewindStmt(t, e, rewindCreateDDL); err != nil {
		t.Fatalf("%q: %v", rewindCreateDDL, err)
	}
	tx, err := e.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}

	var (
		wg       sync.WaitGroup
		stop     atomic.Bool
		stmtsRun atomic.Int64
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			res, serr := tx.Exec(`MATCH (n:Person) WHERE n.tag = 'k3' RETURN n.name`, nil)
			if serr != nil {
				return
			}
			for res.Next() {
			}
			_ = res.Close()
			stmtsRun.Add(1)
		}
	}()

	done := make(chan error, 1)
	go func() {
		fs.failNextSync.Store(true)
		done <- rewindStmt(t, e, rewindDropDDL)
	}()

	select {
	case derr := <-done:
		if derr == nil {
			t.Errorf("%q succeeded although its fsync was armed to fail", rewindDropDDL)
		}
	case <-time.After(20 * time.Second):
		stop.Store(true)
		wg.Wait()
		t.Fatalf("the failing DROP CONSTRAINT did not complete within 20s while an explicit " +
			"transaction was executing statements. An exclusion was added: the rewind must " +
			"take the visibility barrier only, never the schema gate on the transaction's " +
			"behalf (rmp #2738)")
	}
	stop.Store(true)
	wg.Wait()
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if stmtsRun.Load() == 0 {
		t.Fatalf("the explicit transaction executed no statement at all, so this test never " +
			"exercised the interleaving it claims to bound")
	}
}

// TestRewindConstraintDrop_EnforcementIsCorrectWhileTheTransactionIsStillOpen
// asserts the value-set in BOTH directions at the instant it matters most: with
// the eager transaction still OPEN, before any rollback has had a chance to tidy
// up after the rewind.
//
// It exists because the post-rollback accept direction does not move. Measured on
// the neutralised (live-read) build: immediately after the rewind a write of the
// GHOST value was refused as a duplicate, and after [ExplicitTx.Rollback] it was
// not — the rollback releases the transaction's reserved value from whatever
// value-set is registered at that moment, so the phantom is transient in the
// value-set even though it is permanent in the backing index. Asserting the
// accept direction only after the rollback would therefore assert a cell no
// change can move. Here both directions move: on the neutralised build the ghost
// is REFUSED and rewindReal is NOT, and on the fixed build it is the other way
// round, which is the correct answer in both cases — no committed node holds the
// ghost, and k7 committedly holds rewindReal.
func TestRewindConstraintDrop_EnforcementIsCorrectWhileTheTransactionIsStillOpen(t *testing.T) {
	t.Parallel()
	e, fs, _ := newRewindEngine(t)
	ctx := context.Background()
	if err := rewindStmt(t, e, rewindCreateDDL); err != nil {
		t.Fatalf("%q: %v", rewindCreateDDL, err)
	}
	tx, err := e.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	eager := `MATCH (n:Person) WHERE n.tag = 'k7' SET n.name = '` + rewindGhost + `'`
	sres, serr := tx.Exec(eager, nil)
	if serr != nil {
		t.Fatalf("eager %q: %v", eager, serr)
	}
	for sres.Next() {
	}
	if derr := sres.Err(); derr != nil {
		t.Fatalf("eager drain: %v", derr)
	}
	_ = sres.Close()

	fs.failNextSync.Store(true)
	if derr := rewindStmt(t, e, rewindDropDDL); derr == nil {
		t.Fatalf("%q succeeded although its fsync was armed to fail", rewindDropDDL)
	}

	// The transaction is STILL OPEN here. Both probes are autocommit writes, so
	// each carries the constraint's verdict and nothing else of this transaction.
	ghostErr := rewindStmt(t, e, `CREATE (:Person {tag: 'probe', name: '`+rewindGhost+`'})`)
	if errors.Is(ghostErr, exec.ErrConstraintViolation) {
		t.Errorf("with the eager transaction still open, a write of %q was REFUSED as a "+
			"duplicate although no committed node holds it (err=%v). The re-seeded value-set "+
			"holds a value only the uncommitted transaction wrote", rewindGhost, ghostErr)
	}
	realErr := rewindStmt(t, e, `CREATE (:Person {tag: 'probe', name: '`+rewindReal+`'})`)
	if !errors.Is(realErr, exec.ErrConstraintViolation) {
		t.Errorf("with the eager transaction still open, a write of the COMMITTED value %q was "+
			"NOT refused by the UNIQUE constraint (err=%v). The re-seeded value-set lost a "+
			"value the committed graph holds, so a genuine duplicate passes enforcement",
			rewindReal, realErr)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
}
