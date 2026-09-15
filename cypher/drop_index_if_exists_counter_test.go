package cypher_test

// drop_index_if_exists_counter_test.go — regression gate for rmp #2818: a
// `DROP INDEX <name> IF EXISTS` that removes nothing must report nothing.
//
// The counters are the statement's report of the effect it APPLIED, not of the
// effect it attempted. `CREATE INDEX … IF NOT EXISTS` already followed that rule
// (an existing index yields no counters at all), and the durability side of DROP
// already followed it too (an absorbed IF EXISTS emits no WAL record) — but both
// branches of runDropIndex passed the indexes-removed bump to runDDLOpCounted
// UNCONDITIONALLY, so `DROP INDEX <missing> IF EXISTS` reported
// indexesRemoved: 1 while the index listing was identical before and after.
//
// Both branches are covered here because they are separate code paths with
// separate existence checks: the in-memory engine, and the WAL-backed store
// (where the existence flag already existed but fed only the WAL decision).
//
// Layer: short.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// runDDLCounted executes a DDL statement and returns its counters, which are nil
// when the statement recorded no schema effect.
func runDDLCounted(t *testing.T, eng *cypher.Engine, query string) *exec.QueryCounters {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("RunInTx(%q): %v", query, err)
	}
	for res.Next() { // drain
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", query, err)
	}
	c := res.Counters()
	if err := res.Close(); err != nil {
		t.Fatalf("Close(%q): %v", query, err)
	}
	return c
}

// assertIndexesRemoved asserts the indexes-removed effect the statement
// reported. A want of 0 means the statement must report NO removal — either nil
// counters or a zero field; both are "nothing applied".
func assertIndexesRemoved(t *testing.T, query string, c *exec.QueryCounters, want int64) {
	t.Helper()
	var got int64
	if c != nil {
		got = c.IndexesRemoved
	}
	if got != want {
		t.Errorf("%s reported indexesRemoved = %d, want %d", query, got, want)
	}
}

// showIndexCount returns how many indexes SHOW INDEXES lists, so the counter can
// be checked against the observable schema rather than against itself.
func showIndexCount(t *testing.T, eng *cypher.Engine) int {
	t.Helper()
	rows, err := runEntityProp(eng, `SHOW INDEXES`)
	if err != nil {
		t.Fatalf("SHOW INDEXES: %v", err)
	}
	return len(rows)
}

// dropIndexCounterCases is the shared behaviour both engine flavours must show.
// Each step names the statement, the indexes-removed effect it must report, and
// the index count that must be observable afterwards.
type dropIndexStep struct {
	query string
	// isDrop marks the DROP steps, the ones whose indexes-removed report is
	// under test; the CREATE steps exist only to put an index there to remove.
	isDrop bool
	// wantRemoved is the indexes-removed effect the statement must report, and
	// wantIndexCount the number of indexes SHOW INDEXES must list afterwards.
	wantRemoved    int64
	wantIndexCount int
}

var dropIndexCounterSteps = []dropIndexStep{
	// The defect: absorbing a name that never existed removes nothing, so it
	// must report nothing.
	{`DROP INDEX missing_idx IF EXISTS`, true, 0, 0},
	{`CREATE INDEX real_idx FOR (n:L) ON (n.p)`, false, 0, 1},
	// A real removal still reports 1 — the fix must not silence the true case.
	{`DROP INDEX real_idx IF EXISTS`, true, 1, 0},
	// Repeating it is now an absorption, so it reports nothing.
	{`DROP INDEX real_idx IF EXISTS`, true, 0, 0},
	// And the non-IF-EXISTS spelling is unaffected.
	{`CREATE INDEX plain_idx FOR (n:L) ON (n.q)`, false, 0, 1},
	{`DROP INDEX plain_idx`, true, 1, 0},
}

// TestDropIndexIfExists_CounterIsAppliedNotAttempted_InMemory drives the
// no-store branch of runDropIndex.
//
// It fails on the pre-fix engine at the very first step: `DROP INDEX
// missing_idx IF EXISTS` reported indexesRemoved = 1 with SHOW INDEXES empty
// both before and after.
func TestDropIndexIfExists_CounterIsAppliedNotAttempted_InMemory(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	runDropIndexSteps(t, eng)
}

// TestDropIndexIfExists_CounterIsAppliedNotAttempted_WALStore drives the
// store-backed branch, which computes the existence flag under the writer lock
// and previously used it only to decide whether to append a WAL record.
func TestDropIndexIfExists_CounterIsAppliedNotAttempted_WALStore(t *testing.T) {
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
	runDropIndexSteps(t, cypher.NewEngineWithStore(store))
}

// runDropIndexSteps drives [dropIndexCounterSteps] against eng, asserting both
// the reported effect and the observable index listing after each step.
func runDropIndexSteps(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	for _, step := range dropIndexCounterSteps {
		before := showIndexCount(t, eng)
		c := runDDLCounted(t, eng, step.query)
		after := showIndexCount(t, eng)

		if after != step.wantIndexCount {
			t.Fatalf("%s left %d indexes, want %d", step.query, after, step.wantIndexCount)
		}
		if !step.isDrop {
			continue // a CREATE step is setup; only its resulting count matters
		}
		assertIndexesRemoved(t, step.query, c, step.wantRemoved)
		// The counter must agree with what the listing actually LOST. This is
		// the assertion the defect could not satisfy: it reported a removal
		// across a listing that did not shrink.
		if lost := int64(before - after); lost != step.wantRemoved {
			t.Fatalf("%s: listing lost %d indexes but the statement reported %d removed",
				step.query, lost, step.wantRemoved)
		}
		// An absorbed IF EXISTS applied nothing at all, so the statement must
		// not claim to contain updates either.
		if step.wantRemoved == 0 && c.ContainsUpdates() {
			t.Errorf("%s reported ContainsUpdates although it removed nothing", step.query)
		}
	}
}
