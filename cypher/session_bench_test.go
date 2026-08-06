package cypher_test

// session_bench_test.go — the read-path cost of the session guarantee (rmp #2329,
// acceptance criterion 3).
//
// The wait is on the READ side, so a read that follows this session's own write may
// block. These four benchmarks price it against the sessionless path in the two
// shapes that matter: a read-after-write loop, where the session has something to
// wait for, and a pure read loop, where it has nothing and the wait should collapse
// to one atomic load.
//
// lpg.mvcc.sessions.waiting is the gauge that says whether the wait is actually
// happening in production.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func benchEngine(b *testing.B) *cypher.Engine {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	b.Cleanup(func() { _ = g.Close() })
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	for i := 0; i < 500; i++ {
		if _, err := eng.RunInTx(ctx, fmt.Sprintf("CREATE (:P {i: %d})", i), nil); err != nil {
			b.Fatal(err)
		}
	}
	return eng
}

// BenchmarkReadAfterWrite_Sessionless is the baseline arm: the engine's sessionless
// contract, no frontier wait.
func BenchmarkReadAfterWrite_Sessionless(b *testing.B) {
	eng := benchEngine(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := eng.RunInTx(ctx, "CREATE (:W)", nil); err != nil {
			b.Fatal(err)
		}
		res, err := eng.Run(ctx, "MATCH (n:P) RETURN count(n) AS n", nil)
		if err != nil {
			b.Fatal(err)
		}
		for res.Next() {
		}
		_ = res.Close()
	}
}

// BenchmarkReadAfterWrite_Session is the same workload through a session, which pays
// the frontier wait on every read.
func BenchmarkReadAfterWrite_Session(b *testing.B) {
	eng := benchEngine(b)
	ctx := context.Background()
	s := eng.NewSession()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.RunInTx(ctx, "CREATE (:W)", nil); err != nil {
			b.Fatal(err)
		}
		res, err := s.Run(ctx, "MATCH (n:P) RETURN count(n) AS n", nil)
		if err != nil {
			b.Fatal(err)
		}
		for res.Next() {
		}
		_ = res.Close()
	}
}

// BenchmarkReadOnly_Sessionless and _Session isolate the wait's cost on a pure read
// path, where the session has committed nothing and the wait is a bare atomic load.
func BenchmarkReadOnly_Sessionless(b *testing.B) {
	eng := benchEngine(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.Run(ctx, "MATCH (n:P) RETURN count(n) AS n", nil)
		if err != nil {
			b.Fatal(err)
		}
		for res.Next() {
		}
		_ = res.Close()
	}
}

func BenchmarkReadOnly_Session(b *testing.B) {
	eng := benchEngine(b)
	ctx := context.Background()
	s := eng.NewSession()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := s.Run(ctx, "MATCH (n:P) RETURN count(n) AS n", nil)
		if err != nil {
			b.Fatal(err)
		}
		for res.Next() {
		}
		_ = res.Close()
	}
}
