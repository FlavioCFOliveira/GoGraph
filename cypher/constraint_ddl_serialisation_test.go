package cypher_test

// constraint_ddl_serialisation_test.go — regression coverage for #1341
// (CREATE CONSTRAINT validate→register TOCTOU and non-durable registration
// divergence).
//
// (a) TOCTOU: runCreateConstraint used to scan → validate → register → seed
// with no writer exclusion, so a duplicate committed by a concurrent writer
// between scanLabelProperty and SeedUniqueValues left a UNIQUE constraint
// active over already-violating data, with the duplicate absent from the
// value-set and therefore never detected. The fix runs the whole sequence
// under the writer serialisation (the store's single-writer transaction for a
// WAL-backed engine, Engine.writeMu for a store-less one), so the concurrent
// duplicate either commits before the validation scan (CREATE CONSTRAINT is
// rejected) or is itself rejected by the enforced constraint.
//
// (b) Non-durable divergence: when the WAL append of the constraint op fails,
// the constraint used to stay registered (and enforced) in memory while
// nothing reached disk — enforcement silently vanished on the next reopen.
// The fix deregisters the constraint (registry entry, value-set, and backing
// index) before surfacing the error, so the in-memory registry never diverges
// from the durable state: registered ⇔ durable.
//
// Layer: short. goleak-clean (engines/graphs/WAL are local and closed).

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/testfs"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// csCountProp returns the number of :T nodes whose prop equals "dup". It
// deliberately projects the property and counts client-side instead of using
// `WHERE n.<prop> = 'dup'`: an equality predicate on a (label, prop) under a
// UNIQUE constraint is rewritten by the planner to a NodeByIndexSeek over the
// constraint's backing hash index, which is never populated — so the seek
// silently returns zero rows (pre-existing planner coverage gap, same class
// as #1340; tracked separately). The label-scan projection below is immune.
func csCountProp(t *testing.T, eng *cypher.Engine, prop string) int {
	t.Helper()
	q := fmt.Sprintf(`MATCH (n:T) RETURN n.%s AS v`, prop)
	res, err := eng.RunAny(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	defer func() {
		if cerr := res.Close(); cerr != nil {
			t.Errorf("count query Close: %v", cerr)
		}
	}()
	n := 0
	for res.Next() {
		if v, ok := res.Record()["v"].(expr.StringValue); ok && string(v) == "dup" {
			n++
		}
	}
	if rerr := res.Err(); rerr != nil {
		t.Fatalf("count query drain: %v", rerr)
	}
	return n
}

// runCreateConstraintTOCTOU races a duplicate-committing writer against
// CREATE CONSTRAINT for iters rounds and asserts the #1341 (a) invariant on
// every round: when the constraint ends up active, exactly one node carries
// the duplicated value (the racing duplicate was either observed by the
// validation scan — rejecting the CREATE — or rejected by the enforced
// constraint). Before the fix the writer could commit inside the
// scan→register→seed window, leaving the constraint active over two
// duplicates that the value-set never saw.
//
// bulkSeed is the number of filler :T nodes created before the race loop.
// A larger bulkSeed widens the validation scan (O(N) over the label) so the
// pre-fix race window is wide enough to hit reliably. The correctness
// invariant is timing-independent so smaller values still prove correctness;
// they are used under testing.Short to keep CI runtime bounded.
func runCreateConstraintTOCTOU(t *testing.T, eng *cypher.Engine, iters, bulkSeed int) {
	t.Helper()
	seedQ := fmt.Sprintf(`UNWIND range(1, %d) AS x CREATE (:T {filler: x})`, bulkSeed)
	if err := cdRunOne(t, eng, seedQ); err != nil {
		t.Fatalf("bulk seed: %v", err)
	}
	for i := 0; i < iters; i++ {
		prop := fmt.Sprintf("p%d", i)
		seed := fmt.Sprintf(`CREATE (:T {%s: 'dup'})`, prop)
		if err := cdRunOne(t, eng, seed); err != nil {
			t.Fatalf("iter %d: seed: %v", i, err)
		}

		// Race a second, duplicate-value writer against CREATE CONSTRAINT.
		// The deterministic stagger sprays the writer's commit across the
		// DDL's scan→register→seed window over the iterations.
		var wg sync.WaitGroup
		var dupErr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i%8) * 150 * time.Microsecond)
			dupErr = cdRunOne(t, eng, seed)
		}()
		createErr := cdRunOne(t, eng,
			fmt.Sprintf(`CREATE CONSTRAINT u%d ON (n:T) ASSERT n.%s IS UNIQUE`, i, prop))
		wg.Wait()

		if createErr != nil {
			// The duplicate landed before the validation scan: CREATE
			// CONSTRAINT must reject cleanly with a constraint violation.
			if !errors.Is(createErr, exec.ErrConstraintViolation) {
				t.Fatalf("iter %d: CREATE CONSTRAINT failed with %v; want a clean exec.ErrConstraintViolation rejection", i, createErr)
			}
			continue
		}
		// The constraint is active: the graph must hold exactly ONE node with
		// the constrained value. Two nodes means the racing duplicate slipped
		// through the scan→seed window — a UNIQUE constraint enforced over
		// already-violating data (the #1341 (a) corruption).
		if n := csCountProp(t, eng, prop); n != 1 {
			t.Fatalf("iter %d: UNIQUE constraint is active over %d nodes with the same value (writer err: %v); validate→register→seed raced a concurrent writer", i, n, dupErr)
		}
	}
}

// TestCreateConstraint_ConcurrentDuplicate_InMemory is the #1341 (a) gate on
// a store-less engine: the writer serialisation is Engine.writeMu.
//
// The full 80-iteration run widens the scan window via a 4 000-node bulk seed
// to reliably expose the TOCTOU window on slow hardware. Under testing.Short
// (e.g. make smoke, or CI with -short) the seed is reduced to 200 nodes and
// only 10 iterations are executed — enough to exercise the correctness
// invariant without saturating a 2-CPU GitHub runner under the race detector.
func TestCreateConstraint_ConcurrentDuplicate_InMemory(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	iters, seed := 80, 4_000
	if testing.Short() {
		iters, seed = 10, 200
	}
	runCreateConstraintTOCTOU(t, eng, iters, seed)
}

// TestCreateConstraint_ConcurrentDuplicate_WALBacked is the #1341 (a) gate on
// a WAL-backed engine: the writer serialisation is the store's single-writer
// transaction, which the durable constraint op commits on.
func TestCreateConstraint_ConcurrentDuplicate_WALBacked(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() {
		if cerr := w.Close(); cerr != nil {
			t.Errorf("wal.Close: %v", cerr)
		}
	}()
	g := lpg.New[string, float64](adjlist.Config{})
	store := txn.NewStoreWithOptions[string, float64](g, w, cdStoreOpts())
	eng := cypher.NewEngineWithStore(store)
	iters, seed := 25, 4_000
	if testing.Short() {
		iters, seed = 5, 200
	}
	runCreateConstraintTOCTOU(t, eng, iters, seed)
}

// TestCreateConstraint_WALAppendFailureUnwindsRegistration is the #1341 (b)
// gate: when the WAL append of the constraint op fails, the in-memory
// registration must be unwound — the constraint is not enforced in the
// current session, its backing index is gone, and after recovery.Open the
// constraint is absent. Before the fix the constraint stayed registered (and
// enforced) in memory while nothing was durable, so enforcement silently
// vanished on the next reopen (registered ⇎ durable).
func TestCreateConstraint_WALAppendFailureUnwindsRegistration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	ff, err := testfs.New(filepath.Join(dir, "wal"), testfs.Faults{ReturnEIOOnSync: true})
	if err != nil {
		t.Fatalf("testfs.New: %v", err)
	}
	w, err := wal.OpenWith(ff)
	if err != nil {
		t.Fatalf("wal.OpenWith: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{})
	store := txn.NewStoreWithOptions[string, float64](g, w, cdStoreOpts())
	eng := cypher.NewEngineWithStore(store)

	createErr := cdRunOne(t, eng, `CREATE CONSTRAINT u_email ON (n:User) ASSERT n.email IS UNIQUE`)
	if createErr == nil {
		t.Fatal("CREATE CONSTRAINT reported success although the WAL append failed; the registration is not durable")
	}
	if errors.Is(createErr, exec.ErrConstraintViolation) {
		t.Fatalf("CREATE CONSTRAINT failed with a constraint violation (%v); want the WAL append error", createErr)
	}

	// (1) In-memory unwind: no constraint registered, backing index dropped.
	if specs := eng.ConstraintSpecsForSnapshot(); len(specs) != 0 {
		t.Fatalf("constraint registry still holds %d spec(s) after the WAL append failed: %+v; in-memory state diverges from durable state", len(specs), specs)
	}
	if _, gerr := g.IndexManager().GetIndex(exec.UniqueIndexName("User", "email")); !errors.Is(gerr, index.ErrIndexNotFound) {
		t.Fatalf("backing index still registered after the WAL append failed (GetIndex err: %v)", gerr)
	}

	// (2) Enforcement-level: a duplicate pair must NOT trip the phantom
	// constraint. The eager write-path check runs before the (poisoned) WAL
	// commit, so a still-registered constraint would surface as
	// exec.ErrConstraintViolation here; the expected failure is the WAL one.
	dupErr := cdRunOne(t, eng, `CREATE (:User {email: 'x'}), (:User {email: 'x'})`)
	if errors.Is(dupErr, exec.ErrConstraintViolation) {
		t.Fatalf("phantom constraint is still enforced in the current session: %v", dupErr)
	}

	// (3) Durable state: nothing constraint-shaped reaches recovery.
	if cerr := w.Close(); cerr != nil {
		// The poisoned writer may surface the sticky sync error on Close;
		// that is the documented fail-stop contract, not a test failure.
		t.Logf("wal.Close on poisoned writer: %v", cerr)
	}
	res, rerr := recovery.Open[string, float64](dir, cdRecOpts())
	if rerr != nil {
		t.Fatalf("recovery.Open: %v", rerr)
	}
	if len(res.Constraints) != 0 {
		t.Fatalf("recovery surfaced %d constraint(s) after a failed CREATE CONSTRAINT: %+v", len(res.Constraints), res.Constraints)
	}
}
