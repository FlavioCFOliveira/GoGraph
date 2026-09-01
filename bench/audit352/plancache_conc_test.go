package audit352_test

import (
	"context"
	"fmt"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// The plan cache is a single-mutex LRU (cypher/plan_cache.go:55-60). Its own
// doc comment argues the single mutex is acceptable "because plan-cache
// lookups are not on the row-level hot path: they happen once per query
// invocation, gating the per-query work that dominates the total runtime by
// orders of magnitude".
//
// A mutex profile of a 640-goroutine read workload put 99.94% of ALL measured
// mutex contention in planCache.get — yet throughput did not collapse, because
// that workload's queries each did ~2.5 ms of scanning. That is exactly what
// the comment predicts.
//
// The comment's premise is "per-query work dominates". This benchmark tests
// the premise where it is weakest: queries whose body is microseconds, not
// milliseconds. If the lock is the serialisation point, SHORT queries must
// stop scaling with cores while LONG ones keep scaling. If short queries scale
// just as well as long ones, the lock is not limiting anything and the finding
// must be withdrawn.
//
//	go test -run='^$' -bench='^BenchmarkPlanCacheScaling$' -count=5 ./bench/audit352/

var planCacheShapes = []struct {
	name string
	// query is executed once per operation.
	query string
	// wantRows is asserted on every operation.
	wantRows int
}{
	// Microsecond bodies: the plan-cache lookup is a large fraction of the work.
	{"tiny_noop", `RETURN 1 AS x`, 1},
	{"tiny_unwind10", `UNWIND range(1,10) AS i RETURN i`, 10},
	// Millisecond body: the plan-cache lookup is a rounding error.
	{"long_scan", `MATCH (p:Person) WHERE p.bucket < 2 RETURN p.salary`, nodeCount * 2 / 100},
}

// BenchmarkPlanCacheScaling measures throughput of each shape at increasing
// GOMAXPROCS with one goroutine per P. Reported ns/op is per operation across
// all goroutines, so PERFECT scaling holds ns/op constant as cores increase;
// a serialisation point makes ns/op rise.
func BenchmarkPlanCacheScaling(b *testing.B) {
	orig := runtime.GOMAXPROCS(0)
	defer runtime.GOMAXPROCS(orig)
	engine := cypher.NewEngine(benchGraph)
	for _, s := range planCacheShapes {
		s := s
		b.Run(s.name, func(b *testing.B) {
			for _, procs := range []int{1, 2, 4, 8, 10} {
				procs := procs
				b.Run(fmt.Sprintf("procs=%02d", procs), func(b *testing.B) {
					runtime.GOMAXPROCS(procs)
					defer runtime.GOMAXPROCS(orig)
					ctx := context.Background()
					b.ReportAllocs()
					b.SetParallelism(1) // exactly one goroutine per P
					b.ResetTimer()
					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							res, err := engine.Run(ctx, s.query, nil)
							if err != nil {
								b.Fatalf("Run: %v", err)
							}
							n := 0
							for res.Next() {
								n++
							}
							if e := res.Err(); e != nil {
								b.Fatalf("Err: %v", e)
							}
							if n != s.wantRows {
								b.Fatalf("shipped %d rows, want %d", n, s.wantRows)
							}
							if err := res.Close(); err != nil {
								b.Fatalf("Close: %v", err)
							}
						}
					})
				})
			}
		})
	}
}
