package cypher_test

// merge_race_test.go — T930
//
// TestMerge_ConcurrentSameKey verifies that under the production write path
// (Engine backed by a [txn.Store]) N concurrent MERGE calls for the same
// pattern create exactly one node, not N. The store's single-writer mutex
// serialises [Begin] calls, so two goroutines cannot both observe a
// zero-match result from [exec.NewMergeSearchFnFromPattern] and both fire
// ON CREATE.
//
// Layer: short. Race-clean.

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/txn"
	"gograph/store/wal"
)

// TestMerge_ConcurrentSameKey spawns N goroutines that all issue MERGE on
// the same pattern via a single WAL-backed engine, and asserts that exactly
// one node is created.
func TestMerge_ConcurrentSameKey(t *testing.T) {
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

	const goroutines = 8

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			res, runErr := eng.RunInTxAny(ctx, `MERGE (n:Person {name: "Alice"})`, nil)
			if runErr != nil {
				t.Errorf("RunInTxAny: %v", runErr)
				return
			}
			for res.Next() {
			}
			if closeErr := res.Close(); closeErr != nil {
				t.Errorf("result.Close: %v", closeErr)
			}
		}()
	}
	wg.Wait()

	countRes, err := eng.Run(ctx, `MATCH (n:Person {name: "Alice"}) RETURN count(*) AS c`, nil)
	if err != nil {
		t.Fatalf("MATCH count: %v", err)
	}
	defer func() { _ = countRes.Close() }()
	rows := drainRecords(t, countRes)
	if len(rows) != 1 {
		t.Fatalf("count query returned %d rows, want 1", len(rows))
	}
	got := rows[0]["c"]
	if got == nil {
		t.Fatal("count column missing")
	}
	if s := fmtAny(got); s != "1" {
		t.Errorf("after %d concurrent MERGE: count = %s, want 1", goroutines, s)
	}
}
