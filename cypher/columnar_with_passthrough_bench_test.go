package cypher

// columnar_with_passthrough_bench_test.go — allocation/latency benchmarks for the
// columnar scalar-column passthrough over a WITH-projection (task #2045).
//
// The target shapes chain a columnar scan/filter/projection with a following
// projection that selects/reorders materialised scalar columns. Before #2045 that
// following projection pulled its columnar child row-at-a-time and re-boxed every
// cell at the operator boundary; after it, the cells are copied unboxed.
//
// AggGroupScalar and AggGroupSum benchmark the aggregation-over-columnar shape
// whose columnar grouping-key handling is deferred to a follow-up (the reported
// safe split); they are here as a no-regression baseline for that deferred work.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// benchPassthroughGraph builds an n-node graph whose nodes carry an integer "v"
// (unique) and an integer "g" (v mod 8, a low-cardinality grouping key).
func benchPassthroughGraph(tb testing.TB, n int) *lpg.Graph[string, float64] {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("n%07d", i)
		if err := g.AddNode(key); err != nil {
			tb.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeProperty(key, "v", lpg.Int64Value(int64(i))); err != nil {
			tb.Fatalf("SetNodeProperty v: %v", err)
		}
		if err := g.SetNodeProperty(key, "g", lpg.Int64Value(int64(i%8))); err != nil {
			tb.Fatalf("SetNodeProperty g: %v", err)
		}
	}
	return g
}

func benchDrainQuery(b *testing.B, eng *Engine, query string) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.Run(context.Background(), query, nil)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			b.Fatalf("Err: %v", err)
		}
		_ = res.Close()
	}
}

// BenchmarkColumnarWithPassthrough covers the #2045 target shapes plus the
// deferred aggregation baseline and the P2/P3 projection regression guard.
func BenchmarkColumnarWithPassthrough(b *testing.B) {
	// Silence the one-time non-multigraph WARN so it does not pollute benchstat runs.
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prev)

	const n = 2000
	g := benchPassthroughGraph(b, n)
	eng := NewEngine(g)

	cases := []struct {
		name  string
		query string
	}{
		{"WithFilterPassthrough", "MATCH (n) WHERE n.v>=0 WITH n.v AS v RETURN v"},
		{"WithNoFilterPassthrough", "MATCH (n) WITH n.v AS v RETURN v"},
		{"ProjectScalar", "MATCH (n) RETURN n.v AS v"},
		{"AggGroupScalar", "MATCH (n) RETURN n.g AS g, count(*)"},
		{"AggGroupSum", "MATCH (n) WHERE n.v>=0 WITH n.v AS v RETURN sum(v) AS s"},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			benchDrainQuery(b, eng, tc.query)
		})
	}
}
