package cypher_test

// merge_race_test.go — T930, retargeted by rmp #2304.
//
// # What concurrent MERGE guarantees, and what it does not
//
// This file used to assert one thing: that N concurrent MERGE calls for the same
// pattern create exactly one node, on the strength of the store's single-writer
// mutex serialising [txn.Store.Begin]. That reasoning held only while the write
// path ALSO held the graph's exclusive visibility barrier from the statement's
// first read to its commit's publication. rmp #2304 retired that barrier — the
// ordinary write path is now snapshot-isolated and concurrent — and the store
// semaphore alone does not span the window that matters:
//
//	T1  [semaphore] snapshot s1 · MERGE finds nothing · creates · WAL append
//	    [released]  fsync ................................. publishes commit c1
//	T2              [semaphore] snapshot s2 ...
//
// T2's snapshot is taken while T1 is still inside its fsync, so s2 < c1 and T1's
// node is not yet visible to anybody. T2 legitimately finds nothing and creates a
// second one. Nothing here is a defect: T1's commit is not visible because it is
// not yet DURABLE, and making T2 read it anyway would be a dirty read — it would
// match a node that vanishes if T1's fsync fails.
//
// So the honest contract, and the one the reference engines state:
//
//   - WITHOUT a uniqueness constraint, concurrent MERGE on the same pattern MAY
//     create duplicates. Neo4j documents exactly this and directs users to a
//     constraint; Memgraph likewise. PostgreSQL's INSERT ... ON CONFLICT depends
//     on a unique index for the same reason — the second inserter needs an object
//     to wait on, and without a unique index there is none.
//   - WITH a uniqueness constraint, concurrent MERGE converges on ONE node, and
//     does so without surfacing an error to any caller.
//   - SERIALLY, MERGE is unchanged: repeated MERGE of one pattern yields one node.
//
// All three are asserted below, because a contract with an exception is only
// meaningful if the exception is pinned too: TestMerge_ConcurrentSameKey_Unique
// is the guarantee users are told to rely on, and if it ever regresses to
// duplicates the advice this package gives becomes wrong.
//
// Layer: short. Race-clean.

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// mergeRaceEngine builds the production wiring: a Cypher engine over a WAL-backed
// store, which is the composition whose concurrency this file is about.
func mergeRaceEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	w, err := wal.Open(filepath.Join(t.TempDir(), "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	return cypher.NewEngineWithStore(store)
}

// mergeRaceAliceCount returns how many :Person{name:"Alice"} nodes exist.
func mergeRaceAliceCount(t *testing.T, eng *cypher.Engine) string {
	t.Helper()
	res, err := eng.Run(context.Background(), `MATCH (n:Person {name: "Alice"}) RETURN count(*) AS c`, nil)
	if err != nil {
		t.Fatalf("MATCH count: %v", err)
	}
	defer func() { _ = res.Close() }()
	rows := drainRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("count query returned %d rows, want 1", len(rows))
	}
	got := rows[0]["c"]
	if got == nil {
		t.Fatal("count column missing")
	}
	return fmtAny(got)
}

// mergeRaceFanOut issues `goroutines` concurrent MERGEs of the same pattern and
// returns how many reported no error. Errors are recorded rather than failed on,
// because which callers succeed is exactly what differs between the two arms.
func mergeRaceFanOut(t *testing.T, eng *cypher.Engine, goroutines int) int64 {
	t.Helper()
	ctx := context.Background()
	var (
		wg sync.WaitGroup
		ok atomic.Int64
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			res, runErr := eng.RunInTxAny(ctx, `MERGE (n:Person {name: "Alice"})`, nil)
			if runErr != nil {
				t.Logf("MERGE returned: %v", runErr)
				return
			}
			for res.Next() {
			}
			if closeErr := res.Close(); closeErr != nil {
				t.Logf("MERGE close returned: %v", closeErr)
				return
			}
			ok.Add(1)
		}()
	}
	wg.Wait()
	return ok.Load()
}

// TestMerge_ConcurrentSameKey_Unique is THE guarantee: with a uniqueness
// constraint on the merged property, concurrent MERGE converges on one node and
// every caller succeeds.
//
// It is the arm users are directed to, so it is the arm that must not regress. A
// failure here means the advice in this file's header — and in
// docs/isolation-design.md — has become wrong.
func TestMerge_ConcurrentSameKey_Unique(t *testing.T) {
	t.Parallel()

	eng := mergeRaceEngine(t)
	res, err := eng.RunInTxAny(context.Background(),
		`CREATE CONSTRAINT merge_race_name FOR (p:Person) REQUIRE p.name IS UNIQUE`, nil)
	if err != nil {
		t.Fatalf("CREATE CONSTRAINT: %v", err)
	}
	if closeErr := res.Close(); closeErr != nil {
		t.Fatalf("CREATE CONSTRAINT close: %v", closeErr)
	}

	const goroutines = 8
	okCount := mergeRaceFanOut(t, eng, goroutines)

	if got := mergeRaceAliceCount(t, eng); got != "1" {
		t.Errorf("after %d concurrent MERGE under a UNIQUE constraint: count = %s, want 1",
			goroutines, got)
	}
	// Convergence must not be bought with failures: the constraint is supposed to
	// make the losers MATCH the winner's node, not be rejected. A run where some
	// callers erred would still leave one node behind and would pass the count
	// assertion alone, which is why this is checked separately.
	if okCount != goroutines {
		t.Errorf("only %d of %d concurrent MERGEs succeeded; the UNIQUE constraint is "+
			"supposed to make the losers match the winner, not fail", okCount, goroutines)
	}
}

// TestMerge_ConcurrentSameKey_NoConstraint pins the documented EXCEPTION: with no
// uniqueness constraint, concurrent MERGE may duplicate.
//
// It asserts the weak property that actually holds — every caller succeeds and at
// least one node exists — and deliberately does NOT assert a duplicate count. The
// outcome is a genuine race between snapshot acquisition and commit publication,
// so any exact count would be flaky, and asserting "more than one" would demand a
// duplicate the engine is permitted but not required to create.
//
// What it is for: if a future change makes concurrent MERGE fail outright rather
// than duplicate, or makes it lose a caller's write, this notices.
func TestMerge_ConcurrentSameKey_NoConstraint(t *testing.T) {
	t.Parallel()

	eng := mergeRaceEngine(t)
	const goroutines = 8
	okCount := mergeRaceFanOut(t, eng, goroutines)

	if okCount != goroutines {
		t.Errorf("only %d of %d concurrent MERGEs succeeded; without a constraint a MERGE "+
			"has nothing to conflict on and must not fail", okCount, goroutines)
	}
	got := mergeRaceAliceCount(t, eng)
	if got == "0" {
		t.Errorf("after %d concurrent MERGE: count = 0; at least one node must exist", goroutines)
	}
	// Recorded, not asserted: how many duplicates appear depends on how the
	// snapshots interleave with the fsyncs. Logging it keeps the behaviour visible
	// when this test is read after a change.
	t.Logf("concurrent MERGE with no uniqueness constraint left count=%s (duplicates permitted)", got)
}

// TestMerge_SerialSameKey is the property that did NOT change, kept so a
// regression in ordinary MERGE idempotence cannot hide behind the concurrency
// caveat above.
func TestMerge_SerialSameKey(t *testing.T) {
	t.Parallel()

	eng := mergeRaceEngine(t)
	ctx := context.Background()
	const statements = 8
	for i := 0; i < statements; i++ {
		res, err := eng.RunInTxAny(ctx, `MERGE (n:Person {name: "Alice"})`, nil)
		if err != nil {
			t.Fatalf("MERGE %d: %v", i, err)
		}
		for res.Next() {
		}
		if closeErr := res.Close(); closeErr != nil {
			t.Fatalf("MERGE %d close: %v", i, closeErr)
		}
	}
	if got := mergeRaceAliceCount(t, eng); got != "1" {
		t.Errorf("after %d SERIAL MERGE: count = %s, want 1", statements, got)
	}
}
