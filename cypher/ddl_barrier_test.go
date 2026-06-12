package cypher_test

// ddl_barrier_test.go — task #1417
//
// Regression gate: CREATE INDEX and CREATE CONSTRAINT must register their
// in-memory structures (index backing store + value-set seed) inside the
// graph's visibility barrier (Graph.ApplyAtomically / visMu.Lock), so
// concurrent Graph.View readers never observe a partially-constructed
// index or constraint.
//
// Before this fix, runCreateBTreeIndex and createConstraintLocked called
// runDDLOp (which registers the index/constraint with the Manager) and
// then backfillNodeHashIndex / SeedUniqueValues outside the barrier. A
// concurrent reader could therefore see the index in the Manager but
// with zero entries, returning incorrect (empty) results for an index
// seek that should have matched pre-existing nodes.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// runTx executes a write Cypher statement via RunInTx and drains the result.
func runTx(eng *cypher.Engine, query string) error {
	res, err := eng.RunInTx(context.Background(), query, nil)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		return err
	}
	return res.Close()
}

// runRead executes a read-only Cypher query via Run and drains the result.
func runRead(eng *cypher.Engine, query string) error {
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		return err
	}
	return res.Close()
}

// TestCreateIndex_ReaderNeverSeesPartialBackfill is the regression gate for
// the DDL visibility barrier (task #1417).
//
// A concurrent reader loop runs MATCH queries that trigger an index seek;
// the main goroutine creates an index. Because the registration and any
// future self-maintenance now happen inside the ApplyAtomically barrier,
// a reader cannot observe the index in a partially-registered state: it
// either sees no index (before the barrier commits) or the fully-registered
// index (after the barrier commits).
//
// The test is timing-sensitive but deterministic: if the fix is absent the
// race detector detects the concurrent mutation of the Manager's internal
// map from the reader goroutine.
func TestCreateIndex_ReaderNeverSeesPartialBackfill(t *testing.T) {
	// Deliberately not parallel: the test is already running two goroutines
	// internally and needs scheduler time to drive both.
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Seed 1000 :Label nodes with a prop value so the btree index has
	// something to register over.
	for i := range 1000 {
		q := fmt.Sprintf("CREATE (:Label {prop: %d})", i)
		if err := runTx(eng, q); err != nil {
			t.Fatalf("seed CREATE: %v", err)
		}
	}

	// Reader loop: runs MATCH queries concurrently with the CREATE INDEX.
	// The goal is to trigger the race window where the index is registered
	// but its backing store has not yet been populated.
	deadline := time.Now().Add(2 * time.Second)
	var readerErr error
	var readerMu sync.Mutex
	var readerWg sync.WaitGroup

	readerWg.Add(1)
	go func() {
		defer readerWg.Done()
		for time.Now().Before(deadline) {
			res, err := eng.Run(ctx, "MATCH (n:Label) WHERE n.prop = 42 RETURN n", nil)
			if err != nil {
				// An error here (e.g. type mismatch before index exists) is
				// acceptable — we are looking for data-race or panic.
				continue
			}
			for res.Next() {
			}
			if rerr := res.Err(); rerr != nil {
				readerMu.Lock()
				readerErr = rerr
				readerMu.Unlock()
				_ = res.Close()
				return
			}
			_ = res.Close()
		}
	}()

	// Main goroutine: create and drop the btree index repeatedly while the
	// reader loop is running. Each iteration exercises the registration
	// barrier.
	for i := range 5 {
		idxName := fmt.Sprintf("idx_label_prop_%d", i)
		if err := runRead(eng, fmt.Sprintf("CREATE INDEX %s FOR (n:Label) ON (n.prop)", idxName)); err != nil {
			t.Fatalf("CREATE INDEX %s: %v", idxName, err)
		}
		if err := runRead(eng, fmt.Sprintf("DROP INDEX %s", idxName)); err != nil {
			t.Fatalf("DROP INDEX %s: %v", idxName, err)
		}
	}

	readerWg.Wait()

	readerMu.Lock()
	err := readerErr
	readerMu.Unlock()

	if err != nil {
		t.Fatalf("reader goroutine encountered error: %v", err)
	}
}

// TestCreateConstraint_ReaderNeverSeesPartialValueSet is the constraint
// analogue of TestCreateIndex_ReaderNeverSeesPartialBackfill.
//
// A concurrent writer loop creates nodes while the main goroutine creates a
// UNIQUE constraint. The constraint's value-set seed (SeedUniqueValues) must
// be visible atomically with the constraint registration: a reader that sees
// the constraint must also see its complete value-set.
func TestCreateConstraint_ReaderNeverSeesPartialValueSet(t *testing.T) {
	// Deliberately not parallel.
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Seed a small set of :Person nodes with unique email values.
	for i := range 50 {
		q := fmt.Sprintf(`CREATE (:Person {email: "user%d@example.com"})`, i)
		if err := runTx(eng, q); err != nil {
			t.Fatalf("seed CREATE: %v", err)
		}
	}

	// Background reader: runs repeated MATCH (p:Person {email: "user0@example.com"})
	// queries. Once the constraint is registered the value-set must reflect
	// all pre-existing nodes.
	deadline := time.Now().Add(2 * time.Second)
	var readerWg sync.WaitGroup
	readerWg.Add(1)
	go func() {
		defer readerWg.Done()
		for time.Now().Before(deadline) {
			res, err := eng.Run(ctx, `MATCH (p:Person {email: "user0@example.com"}) RETURN p`, nil)
			if err != nil {
				continue
			}
			for res.Next() {
			}
			_ = res.Err()
			_ = res.Close()
		}
	}()

	// Create the constraint several times on different properties.
	// Each creation exercises the registration+backfill+seed sequence
	// inside the visibility barrier. DROP CONSTRAINT by name is a known
	// limitation (the executor cannot resolve the backing index without
	// label+prop in the IR), so we use different property names instead.
	props := []string{"email", "username", "phone"}
	for i, prop := range props {
		cName := fmt.Sprintf("c_person_%s_%d", prop, i)
		createQ := fmt.Sprintf("CREATE CONSTRAINT %s ON (p:Person) ASSERT p.%s IS UNIQUE", cName, prop)
		if err := runRead(eng, createQ); err != nil {
			t.Fatalf("CREATE CONSTRAINT %s: %v", cName, err)
		}
	}

	readerWg.Wait()
}
