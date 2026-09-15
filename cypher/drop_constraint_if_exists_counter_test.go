package cypher_test

// drop_constraint_if_exists_counter_test.go — behaviour gate for rmp #2820: a
// `DROP CONSTRAINT <name> IF EXISTS` that removes nothing must report nothing.
//
// #2820 was raised from CODE SHAPE while fixing rmp #2818, on the reading that
// cypher/api.go passed `countedDDLResult(func(c) { c.ConstraintsRemoved++ })` on
// an unconditional success path — the structural twin of the DROP INDEX defect.
// THE PREMISE DOES NOT REPRODUCE. The two paths are built differently:
//
//   - DROP INDEX cannot decide the outcome before the operator runs, because
//     exec.DropIndexOp absorbs IF EXISTS itself. runDropIndex therefore probes
//     the manager separately and hands runDDLOpCounted a recorder that #2818 had
//     to teach to return nil (dropIndexCounter).
//   - DROP CONSTRAINT must resolve the NAME to its (kind, label, property) before
//     it can do anything at all, since a by-name drop carries neither in the IR.
//     dropConstraintLocked therefore branches on that resolution: a name that
//     resolves to nothing under IF EXISTS returns emptyDDLResult and never
//     reaches countedDDLResult, so no counter is built.
//
// This file is the evidence for that, and the permanent guard on it: the steps
// below would have failed on a DROP CONSTRAINT written the way DROP INDEX was.
// Every counter assertion is cross-checked against the SHOW CONSTRAINTS listing,
// so it reports what the schema actually LOST rather than agreeing with itself.
//
// Both engine flavours run, because runDropConstraint splits in two before
// dropConstraintLocked: the store-less engine and the WAL-backed one, whose
// branch additionally appends the durable drop op.
//
// Layer: short.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// showConstraintCount returns how many constraints SHOW CONSTRAINTS lists, so a
// reported removal can be checked against the observable schema.
func showConstraintCount(t *testing.T, eng *cypher.Engine) int {
	t.Helper()
	rows, err := runEntityProp(eng, `SHOW CONSTRAINTS`)
	if err != nil {
		t.Fatalf("SHOW CONSTRAINTS: %v", err)
	}
	return len(rows)
}

// dropConstraintStep is one statement of the shared sequence, with the
// constraints-removed effect it must report and the constraint count that must
// be observable afterwards.
type dropConstraintStep struct {
	query string
	// isDrop marks the statements whose constraints-removed report is under
	// test; the CREATE steps exist only to put a constraint there to remove.
	isDrop bool
	// wantRemoved is the constraints-removed effect the statement must report,
	// and wantCount the number of constraints SHOW CONSTRAINTS must list after.
	wantRemoved int64
	wantCount   int
	// wantNotFound marks the one step that must RAISE rather than report: a
	// missing name without IF EXISTS is never a fail-silent success.
	wantNotFound bool
}

var dropConstraintCounterSteps = []dropConstraintStep{
	// The shape #2820 predicted would miscount: absorbing a name that never
	// existed removes nothing, so it must report nothing.
	{query: `DROP CONSTRAINT missing_c IF EXISTS`, isDrop: true, wantRemoved: 0, wantCount: 0},
	{query: `CREATE CONSTRAINT real_c FOR (n:CK) REQUIRE n.k IS UNIQUE`, wantCount: 1},
	// A real removal still reports 1 — the guard must not accept silence here.
	{query: `DROP CONSTRAINT real_c IF EXISTS`, isDrop: true, wantRemoved: 1, wantCount: 0},
	// Repeating it is now an absorption, so it reports nothing.
	{query: `DROP CONSTRAINT real_c IF EXISTS`, isDrop: true, wantRemoved: 0, wantCount: 0},
	// The non-IF-EXISTS spelling is unaffected on a constraint that exists,
	{query: `CREATE CONSTRAINT plain_c FOR (n:CK) REQUIRE n.q IS UNIQUE`, wantCount: 1},
	{query: `DROP CONSTRAINT plain_c`, isDrop: true, wantRemoved: 1, wantCount: 0},
	// and raises on one that does not.
	{query: `DROP CONSTRAINT plain_c`, isDrop: true, wantRemoved: 0, wantCount: 0, wantNotFound: true},
	// NOT NULL has no backing index and its own operator branch, so it is
	// exercised too rather than assumed to follow UNIQUE.
	{query: `CREATE CONSTRAINT nn_c FOR (n:CK) REQUIRE n.r IS NOT NULL`, wantCount: 1},
	{query: `DROP CONSTRAINT nn_c IF EXISTS`, isDrop: true, wantRemoved: 1, wantCount: 0},
	{query: `DROP CONSTRAINT nn_c IF EXISTS`, isDrop: true, wantRemoved: 0, wantCount: 0},
}

// TestDropConstraintIfExists_CounterIsAppliedNotAttempted_InMemory drives the
// store-less branch of runDropConstraint.
func TestDropConstraintIfExists_CounterIsAppliedNotAttempted_InMemory(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	runDropConstraintSteps(t, cypher.NewEngine(g))
}

// TestDropConstraintIfExists_CounterIsAppliedNotAttempted_WALStore drives the
// store-backed branch, which additionally appends a durable drop op.
func TestDropConstraintIfExists_CounterIsAppliedNotAttempted_WALStore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	runDropConstraintSteps(t, cypher.NewEngineWithStore(store))
}

// runDropConstraintSteps drives [dropConstraintCounterSteps] against eng,
// asserting the reported effect, the observable listing, and their agreement.
func runDropConstraintSteps(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	for _, step := range dropConstraintCounterSteps {
		before := showConstraintCount(t, eng)
		c, err := runDDLMaybeFailing(t, eng, step.query)
		after := showConstraintCount(t, eng)

		if step.wantNotFound {
			if !errors.Is(err, exec.ErrConstraintNotFound) {
				t.Fatalf("%s: err = %v, want one wrapping exec.ErrConstraintNotFound", step.query, err)
			}
		} else if err != nil {
			t.Fatalf("%s: %v", step.query, err)
		}
		if after != step.wantCount {
			t.Fatalf("%s left %d constraints, want %d", step.query, after, step.wantCount)
		}
		if !step.isDrop {
			continue // a CREATE step is setup; only its resulting count matters
		}
		var got int64
		if c != nil {
			got = c.ConstraintsRemoved
		}
		if got != step.wantRemoved {
			t.Errorf("%s reported constraintsRemoved = %d, want %d", step.query, got, step.wantRemoved)
		}
		// The counter must agree with what the listing actually LOST. This is the
		// assertion the #2818 shape could not satisfy: it reported a removal
		// across a listing that did not shrink.
		if lost := int64(before - after); lost != step.wantRemoved {
			t.Fatalf("%s: listing lost %d constraints but the statement reported %d removed",
				step.query, lost, step.wantRemoved)
		}
		// An absorbed IF EXISTS applied nothing at all, so the statement must not
		// claim to contain updates either.
		if step.wantRemoved == 0 && c.ContainsUpdates() {
			t.Errorf("%s reported ContainsUpdates although it removed nothing", step.query)
		}
	}
}

// runDDLMaybeFailing executes a DDL statement and returns its counters and the
// first error it raised, at plan time or during iteration. Unlike runDDLCounted
// it tolerates a failure, because one step here is required to raise.
func runDDLMaybeFailing(t *testing.T, eng *cypher.Engine, query string) (*exec.QueryCounters, error) {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), query, nil)
	if err != nil {
		return nil, err
	}
	for res.Next() { // drain
	}
	iterErr := res.Err()
	c := res.Counters()
	if cerr := res.Close(); cerr != nil && iterErr == nil {
		iterErr = cerr
	}
	return c, iterErr
}
