package cypher_test

// build_under_barrier_race_test.go — #1077
//
// Regression reproducer for the concurrent-read+write tear in the Cypher
// physical-plan build. Before the fix, Engine.Run built its operator tree
// (including buildEdgeTypeFilter, which snapshots the forward CSR and then
// iterated the live node space) OUTSIDE the graph's visibility barrier. A
// concurrent RunInTx CREATE that grew the node space could grow the live
// adjacency between the CSR snapshot and the filter loop, so the loop indexed
// the fixed-length snapshot vertices slice out of range and panicked with
// "index out of range [N] with length N". The build now runs INSIDE
// Graph.View / Graph.ApplyAtomically, so readers and writers are serialised by
// visMu and the snapshot can no longer be torn.
//
// The test runs relationship MATCH readers (which exercise the edge-type
// filter build) concurrently with writers that CREATE fresh nodes, growing the
// NodeID space on every iteration. Under `go test -race` it must show no panic
// and no data race.
//
// Layer: short. Race-clean.

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/txn"
	"gograph/store/wal"
)

// TestRun_ConcurrentReadWrite_NoBuildTear spawns relationship-MATCH readers
// concurrently with node-growing writers and asserts that no read panics or
// races against a concurrent node-space growth. The readers run a relationship
// pattern with an explicit type so buildEdgeTypeFilter is on the build path;
// the writers issue RunInTx CREATEs that intern brand-new node keys, growing
// the live adjacency's MaxNodeID on every iteration.
func TestRun_ConcurrentReadWrite_NoBuildTear(t *testing.T) {
	t.Parallel()

	w, err := wal.Open(filepath.Join(t.TempDir(), "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() { _ = w.Close() }()

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	eng := cypher.NewEngineWithStore(store)
	ctx := context.Background()

	// Seed a single KNOWS relationship so the readers have at least one
	// labelled edge to traverse; the count itself is not asserted, only the
	// absence of a panic/race under concurrent growth.
	seed, err := eng.RunInTxAny(ctx, `CREATE (a:Person {k:'seed-a'})-[:KNOWS]->(b:Person {k:'seed-b'})`, nil)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	for seed.Next() {
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}

	const (
		readers    = 8
		writers    = 4
		iterations = 400
	)

	var (
		writersWG sync.WaitGroup
		readersWG sync.WaitGroup
		nextKey   atomic.Int64
		stop      atomic.Bool
	)

	// Writers: each iteration interns a brand-new node key, growing the
	// NodeID space (which GROWS the live adjacency under visMu.Lock).
	writersWG.Add(writers)
	for wkr := 0; wkr < writers; wkr++ {
		go func() {
			defer writersWG.Done()
			for i := 0; i < iterations; i++ {
				key := nextKey.Add(1)
				res, runErr := eng.RunInTxAny(ctx,
					`CREATE (n:Person {k:$k})`,
					map[string]any{"k": fmt.Sprintf("grow-%d", key)},
				)
				if runErr != nil {
					t.Errorf("writer RunInTxAny: %v", runErr)
					return
				}
				for res.Next() {
				}
				if closeErr := res.Close(); closeErr != nil {
					t.Errorf("writer result.Close: %v", closeErr)
					return
				}
			}
		}()
	}

	// Readers: relationship MATCH with an explicit type drives the edge-type
	// filter build. They loop until every writer has finished so reads and
	// node-space growth overlap for the whole run.
	readersWG.Add(readers)
	for rdr := 0; rdr < readers; rdr++ {
		go func() {
			defer readersWG.Done()
			for !stop.Load() {
				res, runErr := eng.Run(ctx, `MATCH ()-[r:KNOWS]->() RETURN count(r) AS c`, nil)
				if runErr != nil {
					t.Errorf("reader Run: %v", runErr)
					return
				}
				for res.Next() {
				}
				if closeErr := res.Close(); closeErr != nil {
					t.Errorf("reader result.Close: %v", closeErr)
					return
				}
			}
		}()
	}

	// Join the writers first, then signal the readers to stop and join them.
	// Reads and node-space growth therefore overlap for the entire writer run.
	writersWG.Wait()
	stop.Store(true)
	readersWG.Wait()
}
