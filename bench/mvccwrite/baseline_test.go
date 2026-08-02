package mvccwrite

// Baseline instrument for the multi-writer MVCC sprint: how does engine write
// throughput scale with the number of concurrent writer goroutines?
//
// Today the answer is not merely flat, it is NEGATIVE, because a Cypher write
// takes lpg.Graph.visMu exclusively for the whole apply. This file exists to
// record that baseline as a number before any change is made.
//
// # Baseline, entry head 6b990377, Apple M4 (10 cores), darwin/arm64
//
// go test -run='^$' -bench=BenchmarkWriteScaling -benchtime=3000x
//
//	writers   commits/s   ns/commit   scaling
//	      1     333 590       2 998     1.00x
//	      2     303 552       3 294     0.91x
//	      4     283 953       3 522     0.85x
//	      8     278 919       3 585     0.84x
//	     16     276 043       3 613     0.83x
//
// Sixteen writers deliver 0.83x the throughput of one on a ten-core machine.
// No store is attached, so this isolates the concurrency-control ceiling from
// the WAL fsync entirely: what it measures is the barrier and nothing else.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func newEngine(tb testing.TB) *cypher.Engine {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	return cypher.NewEngine(g)
}

// BenchmarkWriteScaling measures commits/second at increasing writer counts.
func BenchmarkWriteScaling(b *testing.B) {
	for _, writers := range []int{1, 2, 4, 8, 16} {
		b.Run(fmt.Sprintf("writers=%d", writers), func(b *testing.B) {
			eng := newEngine(b)
			ctx := context.Background()
			var seq atomic.Int64
			var commits atomic.Int64

			b.ResetTimer()
			start := time.Now()
			var wg sync.WaitGroup
			perWriter := b.N / writers
			if perWriter < 1 {
				perWriter = 1
			}
			for w := 0; w < writers; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < perWriter; i++ {
						id := seq.Add(1)
						res, err := eng.RunInTx(ctx,
							"CREATE (n:Account {id: $id})",
							map[string]expr.Value{"id": expr.IntegerValue(id)})
						if err != nil {
							b.Error(err)
							return
						}
						if err := res.Err(); err != nil {
							b.Error(err)
							return
						}
						_ = res.Close()
						commits.Add(1)
					}
				}()
			}
			wg.Wait()
			elapsed := time.Since(start)
			b.StopTimer()
			n := commits.Load()
			b.ReportMetric(float64(n)/elapsed.Seconds(), "commits/s")
			b.ReportMetric(float64(elapsed.Nanoseconds())/float64(n), "ns/commit")
		})
	}
}
