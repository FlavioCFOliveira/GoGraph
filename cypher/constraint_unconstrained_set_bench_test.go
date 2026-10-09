package cypher

// constraint_unconstrained_set_bench_test.go — concurrent SET of a key no
// constraint names, while a UNIQUE constraint exists on another key (rmp #3082).
//
// Each goroutine writes its own node, so MVCC never refuses a write and the
// measured contention is the engine's, not the workload's. Before rmp #3082
// every one of these writes took the constraint registry's exclusive lock.
//
// Run with GOGRAPH_FSYNC_LATENCY=off; the engine is store-less.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func BenchmarkConcurrentSetUnconstrainedKey(b *testing.B) {
	for _, workers := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("goroutines=%d", workers), func(b *testing.B) {
			ctx := context.Background()
			eng := NewEngine(lpg.New[string, float64](adjlist.Config{}))
			if err := straddleRun(eng, `CREATE CONSTRAINT u FOR (n:Acct) REQUIRE n.email IS UNIQUE`); err != nil {
				b.Fatal(err)
			}
			if err := straddleRun(eng, fmt.Sprintf(
				`UNWIND range(0, %d) AS i CREATE (:Acct {id: i, email: 'e' + toString(i), bal: 0})`, workers-1)); err != nil {
				b.Fatal(err)
			}
			const q = `MATCH (n:Acct {id: $id}) SET n.bal = $v`
			b.ReportAllocs()
			b.ResetTimer()
			var wg sync.WaitGroup
			errs := make(chan error, workers)
			for w := 0; w < workers; w++ {
				n := b.N / workers
				if w < b.N%workers {
					n++
				}
				wg.Add(1)
				go func(w, n int) {
					defer wg.Done()
					params := map[string]expr.Value{"id": expr.IntegerValue(w)}
					for i := 0; i < n; i++ {
						params["v"] = expr.IntegerValue(i)
						res, err := eng.RunInTx(ctx, q, params)
						if err == nil {
							for res.Next() {
							}
							err = res.Err()
							_ = res.Close()
						}
						if err != nil {
							errs <- err
							return
						}
					}
				}(w, n)
			}
			wg.Wait()
			b.StopTimer()
			close(errs)
			if err := <-errs; err != nil {
				b.Fatal(err)
			}
		})
	}
}
