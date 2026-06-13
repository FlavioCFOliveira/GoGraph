package cypher_test

// isolation_test.go — F3.3 isolation proof for the Cypher engine
// (docs/isolation-design.md). Concurrent Cypher reads (Engine.Run via RunAny)
// and writes (Engine.RunInTx via RunInTxAny) must never let a reader observe a
// partially-applied write transaction. Run/RunInTx now execute the whole query
// under the graph's visibility barrier (Graph.View / Graph.ApplyAtomically) and
// materialise the rows, so a reader sees either none or all of a write
// transaction's effects.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// TestIsolation_Cypher_NoPartialWriteObservable runs one writer that, in a
// single transaction, sets a.v and b.v to the SAME new value, and many readers
// that fetch both and assert they are equal. Without transaction-atomic
// visibility a reader could observe the new a.v and the old b.v. The barrier
// guarantees the two SETs flip together. Run under -race.
func TestIsolation_Cypher_NoPartialWriteObservable(t *testing.T) {
	testlayers.RequireSoak(t) // concurrency isolation stress → soak layer (short-layer per-package budget, #1460)
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Seed two nodes a, b with v = 0.
	seed, err := eng.RunInTxAny(ctx, `CREATE (a:N {k:'a', v:0}), (b:N {k:'b', v:0})`, nil)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	for seed.Next() {
	}
	if err := seed.Err(); err != nil {
		t.Fatalf("seed iterate: %v", err)
	}
	_ = seed.Close()

	const (
		writes  = 3000
		readers = 8
	)
	var (
		wg        sync.WaitGroup
		done      atomic.Bool
		violation atomic.Int64
		reads     atomic.Int64
		failErr   atomic.Value // first error string
	)
	setErr := func(e error) {
		if e != nil {
			failErr.CompareAndSwap(nil, e.Error())
		}
	}

	// Writer: a.v and b.v are set to the same value in one transaction.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer done.Store(true)
		for i := 1; i <= writes; i++ {
			res, err := eng.RunInTxAny(ctx,
				`MATCH (a:N {k:'a'}), (b:N {k:'b'}) SET a.v = $i, b.v = $i`,
				map[string]any{"i": i})
			if err != nil {
				setErr(err)
				return
			}
			for res.Next() {
			}
			if e := res.Err(); e != nil {
				setErr(e)
				_ = res.Close()
				return
			}
			_ = res.Close()
		}
	}()

	// Readers: a.v must always equal b.v within a single (materialised) read.
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !done.Load() {
				res, err := eng.RunAny(ctx,
					`MATCH (a:N {k:'a'}), (b:N {k:'b'}) RETURN a.v AS av, b.v AS bv`, nil)
				if err != nil {
					setErr(err)
					return
				}
				for res.Next() {
					rec := res.Record()
					av, bv := fmt.Sprint(rec["av"]), fmt.Sprint(rec["bv"])
					reads.Add(1)
					if av != bv {
						violation.Add(1)
					}
				}
				if e := res.Err(); e != nil {
					setErr(e)
					_ = res.Close()
					return
				}
				_ = res.Close()
			}
		}()
	}

	wg.Wait()
	if s := failErr.Load(); s != nil {
		t.Fatalf("query error: %v", s)
	}
	if v := violation.Load(); v != 0 {
		t.Fatalf("observed %d partial-transaction violations (a.v != b.v in one read)", v)
	}
	if reads.Load() == 0 {
		t.Fatal("readers never read a row; test did not exercise the invariant")
	}
}
