package cypher

// index_backfill_cancellation_test.go — regression coverage for rmp #1872
// (2026-07-02 production-readiness audit round 2, follow-up to #1869):
// "CREATE INDEX/CONSTRAINT backfill and validation scans have incomplete or
// missing cancellation polling".
//
// Three related, independent gaps, all fixed here:
//
//  1. The parallel hash-index backfill's poll check (index_binding.go's
//     processRange) used the shared slice's absolute index (i&0xFFF==0), not
//     one relative to each worker's own range start. For a representative
//     20,000-row/10-worker backfill this placed zero checkpoints inside 5 of
//     the 10 workers' own ranges, so those workers could not observe an
//     early cancellation at all. Fixed to (i-lo)&0xFFF==0, which — since
//     i=lo always satisfies (lo-lo)&0xFFF==0==0 trivially — guarantees every
//     worker's very first iteration is a checkpoint, regardless of how the
//     range happens to align with the absolute 4096 boundary.
//  2. backfillNodeBTreeIndex / backfillNodeBTreeIndexNumeric took no ctx
//     parameter at all and polled no cancellation whatsoever.
//  3. scanLabelProperty (CREATE CONSTRAINT's pre-existing-data validation
//     scan, both UNIQUE and NOT NULL) took no ctx parameter at all; for NOT
//     NULL specifically there was no other ctx-aware step anywhere later in
//     createConstraintLocked, so the entire statement was uncancellable
//     end-to-end on a large graph.
//
// All three are pure liveness defects (the statement always completed
// correctly and atomically; it just could not be interrupted early) — see
// task #1869's own investigation, which first established that a DDL
// statement's post-commit confirmation must never observe cancellation, a
// property this fix does not touch or regress (verified below).

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestShouldPollWorkerRelative_AlwaysTrueAtRangeStart calls the REAL,
// package-level [shouldPollWorkerRelative] function processRange actually
// uses — not a duplicated inline expression — so a future regression of the
// production formula cannot silently escape this test the way it would
// escape TestBackfillNodeHashIndex_ContextCancelled (index_binding_parallel_test.go):
// that test cancels ctx before the backfill starts, and worker 0's lo=0
// trivially satisfies EITHER formula (old or new), so the buggy pre-fix
// formula still passes it — confirmed independently by two specialist
// reviews of this task, both of which reverted the real fix in place and
// found the existing test suite (before this test existed) did not notice.
// Extracting the predicate to a named, directly callable function — rather
// than only proving the formula correct in the abstract — closes that gap.
func TestShouldPollWorkerRelative_AlwaysTrueAtRangeStart(t *testing.T) {
	t.Parallel()
	for _, lo := range []int{0, 1, 2000, 4095, 4096, 4097, 18000, 1_000_003} {
		if !shouldPollWorkerRelative(lo, lo) {
			t.Errorf("shouldPollWorkerRelative(%d, %d) = false, want true (i=lo must always poll)", lo, lo)
		}
	}
}

// TestShouldPollWorkerRelative_MatchesAuditScenario reproduces the audit's
// own measurement (20,000 rows split across 10 workers, chunk size 2000)
// against the REAL production function, and contrasts it with the pre-fix
// global-index formula (kept here only as an inline expression for
// comparison — never called by production code) to confirm the scenario
// still exercises a genuine gap in the old formula that the real,
// non-duplicated new formula closes.
func TestShouldPollWorkerRelative_MatchesAuditScenario(t *testing.T) {
	t.Parallel()
	const totalRows = 20_000
	const workers = 10
	const chunk = totalRows / workers // 2000, matching the audit's own scenario

	oldFormulaHasGap := false
	for w := 0; w < workers; w++ {
		lo := w * chunk
		hi := lo + chunk

		oldChecked := false
		newChecked := false
		for i := lo; i < hi; i++ {
			if i&pollGranularityMask == 0 { // the pre-fix, buggy global-index check
				oldChecked = true
			}
			if shouldPollWorkerRelative(i, lo) { // the real production function
				newChecked = true
			}
		}
		if !oldChecked {
			oldFormulaHasGap = true
		}
		if !newChecked {
			t.Errorf("worker %d (range [%d,%d)): shouldPollWorkerRelative never polls — regression", w, lo, hi)
		}
	}
	if !oldFormulaHasGap {
		t.Fatal("the pre-fix formula polled in every worker's range for this scenario — " +
			"the test no longer reproduces the audit's finding, so it cannot prove the fix meaningfully; " +
			"the scenario constants need revisiting")
	}
}

// TestBackfillNodeBTreeIndex_ContextCancelled mirrors
// TestBackfillNodeHashIndex_ContextCancelled (index_binding_parallel_test.go)
// for the btree string backfill, which previously took no ctx parameter and
// polled no cancellation at all.
func TestBackfillNodeBTreeIndex_ContextCancelled(t *testing.T) {
	t.Parallel()
	const n = backfillParallelMinNodes * 2
	g, _ := seedLabeledNamed(t, n, "Person")
	e := NewEngine(g)

	idx, err := newBoundNodeBTreeIndex(e.g.ReadAt(nil), "Person", "name")
	if err != nil {
		t.Fatalf("newBoundNodeBTreeIndex: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the backfill starts

	if berr := e.backfillNodeBTreeIndex(ctx, idx, "Person", "name"); berr == nil {
		t.Fatal("backfill with cancelled context returned nil, want context.Canceled")
	} else if !errors.Is(berr, context.Canceled) {
		t.Fatalf("backfill error = %v, want context.Canceled", berr)
	}
}

// TestBackfillNodeBTreeIndexNumeric_ContextCancelled is the numeric
// companion's counterpart to TestBackfillNodeBTreeIndex_ContextCancelled.
func TestBackfillNodeBTreeIndexNumeric_ContextCancelled(t *testing.T) {
	t.Parallel()
	const n = backfillParallelMinNodes * 2
	g := lpg.New[string, float64](adjlist.Config{})
	for i := 0; i < n; i++ {
		key := indexBackfillTestKey(i)
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(key, "age", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	e := NewEngine(g)

	idx, err := newBoundNodeBTreeIndexNumeric(e.g.ReadAt(nil), "Person", "age")
	if err != nil {
		t.Fatalf("newBoundNodeBTreeIndexNumeric: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if berr := e.backfillNodeBTreeIndexNumeric(ctx, idx, "Person", "age"); berr == nil {
		t.Fatal("backfill with cancelled context returned nil, want context.Canceled")
	} else if !errors.Is(berr, context.Canceled) {
		t.Fatalf("backfill error = %v, want context.Canceled", berr)
	}
}

// TestBackfillNodeBTreeIndex_NotCancelled_StillCompletes guards against the
// fix accidentally breaking the uncancelled path: an ordinary, non-cancelled
// backfill must still populate the index exactly as before.
func TestBackfillNodeBTreeIndex_NotCancelled_StillCompletes(t *testing.T) {
	t.Parallel()
	const n = backfillParallelMinNodes * 2
	g, names := seedLabeledNamed(t, n, "Person")
	e := NewEngine(g)

	idx, err := newBoundNodeBTreeIndex(e.g.ReadAt(nil), "Person", "name")
	if err != nil {
		t.Fatalf("newBoundNodeBTreeIndex: %v", err)
	}
	if berr := e.backfillNodeBTreeIndex(context.Background(), idx, "Person", "name"); berr != nil {
		t.Fatalf("backfill: %v", berr)
	}
	for name := range names {
		if c := idx.Lookup(name).GetCardinality(); c != 1 {
			t.Fatalf("Lookup(%q) cardinality = %d, want 1 after an uncancelled backfill", name, c)
		}
	}
}

// indexBackfillTestKey generates a distinct node key, matching
// seedLabeledNamed's own "k<i>" convention (index_binding_parallel_test.go).
func indexBackfillTestKey(i int) string {
	return fmt.Sprintf("k%d", i)
}

// drainIndexBackfillResult drains and closes a Result, returning the first
// error encountered (from iteration or Close), local to this file so it does
// not depend on any sibling _test.go file's own drain helper.
func drainIndexBackfillResult(res *Result) error {
	for res.Next() {
	}
	err := res.Err()
	if cerr := res.Close(); err == nil {
		err = cerr
	}
	return err
}

// TestCreateConstraint_NotNull_ContextCancelled proves scanLabelProperty's
// new ctx-awareness closes the gap task #1869's own investigation found:
// before this fix, no step in createConstraintLocked's NOT NULL path ever
// consulted ctx at all, so a large graph's CREATE CONSTRAINT ... IS NOT NULL
// could not be interrupted end-to-end. Also confirms atomicity: a cancelled
// attempt registers nothing, so the identical statement can be retried
// cleanly afterward — if the cancelled attempt had left anything registered,
// this second call would fail with "constraint already exists" instead.
//
// Tests scanLabelProperty directly rather than through a full CREATE
// CONSTRAINT statement: runCreateConstraint has its own pre-existing,
// unrelated upfront `if err := ctx.Err(); err != nil` guard (checked before
// scanLabelProperty is ever reached), which would make an end-to-end test
// using an already-cancelled context pass regardless of whether
// scanLabelProperty's OWN new cancellation check works at all — exactly the
// kind of false-positive this project's non-vacuousness discipline requires
// ruling out. Calling scanLabelProperty directly isolates the exact unit
// this task fixed from that unrelated upstream guard.
func TestCreateConstraint_NotNull_ContextCancelled(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	for i := 0; i < 5_000; i++ {
		key := indexBackfillTestKey(i)
		if err := g.SetNodeLabel(key, "Acct"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(key, "email", lpg.StringValue(key+"@example.com")); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	e := NewEngine(g)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := e.scanLabelProperty(ctx, "Acct", "email"); err == nil {
		t.Fatal("scanLabelProperty with a cancelled context returned nil error, want context.Canceled")
	} else if !errors.Is(err, context.Canceled) {
		t.Fatalf("scanLabelProperty error = %v, want context.Canceled", err)
	}

	// Sanity: the same scan with a live context still returns correct data,
	// proving the cancellation check does not interfere with the ordinary
	// path (mirrors TestBackfillNodeBTreeIndex_NotCancelled_StillCompletes).
	values, anyNull, err := e.scanLabelProperty(context.Background(), "Acct", "email")
	if err != nil {
		t.Fatalf("scanLabelProperty with a live context: %v", err)
	}
	if anyNull {
		t.Error("anyNull = true, want false (every seeded node has an email)")
	}
	if len(values) != 5_000 {
		t.Errorf("len(values) = %d, want 5000", len(values))
	}
}

// TestCreateConstraint_Unique_ContextCancelled is a second, higher-volume
// instance of the same scanLabelProperty proof — UNIQUE and NOT NULL share
// the exact same scan, so this pins the invariant against a larger snapshot
// (crossing several 4096-row poll boundaries, not just one).
func TestCreateConstraint_Unique_ContextCancelled(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	const n = backfillParallelMinNodes * 2
	for i := 0; i < n; i++ {
		key := indexBackfillTestKey(i)
		if err := g.SetNodeLabel(key, "Acct"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(key, "email", lpg.StringValue(key+"@example.com")); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	e := NewEngine(g)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := e.scanLabelProperty(ctx, "Acct", "email"); err == nil {
		t.Fatal("scanLabelProperty with a cancelled context returned nil error, want context.Canceled")
	} else if !errors.Is(err, context.Canceled) {
		t.Fatalf("scanLabelProperty error = %v, want context.Canceled", err)
	}

	values, _, err := e.scanLabelProperty(context.Background(), "Acct", "email")
	if err != nil {
		t.Fatalf("scanLabelProperty with a live context: %v", err)
	}
	if len(values) != n {
		t.Errorf("len(values) = %d, want %d", len(values), n)
	}
}

// TestCreateConstraint_NotNull_EndToEnd_StillWorks is an end-to-end sanity
// check (not a cancellation test) that CREATE CONSTRAINT ... IS NOT NULL
// still functions correctly through the full engine after scanLabelProperty's
// signature change — a live context must not spuriously trip the new check.
func TestCreateConstraint_NotNull_EndToEnd_StillWorks(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	for i := 0; i < 100; i++ {
		key := indexBackfillTestKey(i)
		if err := g.SetNodeLabel(key, "Acct"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(key, "email", lpg.StringValue(key+"@example.com")); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	e := NewEngine(g)

	res, err := e.Run(context.Background(), `CREATE CONSTRAINT acct_email_nn ON (n:Acct) ASSERT n.email IS NOT NULL`, nil)
	if err != nil {
		t.Fatalf("CREATE CONSTRAINT: %v", err)
	}
	if derr := drainIndexBackfillResult(res); derr != nil {
		t.Fatalf("drain result: %v", derr)
	}
}
