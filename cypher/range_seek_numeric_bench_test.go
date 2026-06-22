package cypher_test

// range_seek_numeric_bench_test.go — #1652 numeric range-seek throughput.
//
// BenchmarkNumericRangeSeek_Seek vs BenchmarkNumericRangeSeek_Scan run the SAME
// selective numeric range query over the SAME ~50k-node mixed integer/float
// :Person graph, the first with the unified numeric companion seek ENABLED and
// the second with it DISABLED (a label scan + residual filter). The contrast in
// ns/op and allocs/op is the empirical evidence that the seek narrows the
// candidate stream rather than scanning the whole label population.
//
// Layer: short (the default). The graph is built once per benchmark in a setup
// phase outside the timed loop.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildNumericBenchGraph builds an n-node :Person graph with alternating
// integer and float "age" values spanning [0, n), indexed for btree range
// seeks.
func buildNumericBenchGraph(b *testing.B, n int) *lpg.Graph[string, float64] {
	b.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for i := 0; i < n; i++ {
		k := "p" + itoa(i)
		if err := g.AddNode(k); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "Person"); err != nil {
			b.Fatalf("SetNodeLabel: %v", err)
		}
		var v lpg.PropertyValue
		if i%2 == 0 {
			v = lpg.Int64Value(int64(i))
		} else {
			v = lpg.Float64Value(float64(i) + 0.5)
		}
		if err := g.SetNodeProperty(k, "age", v); err != nil {
			b.Fatalf("SetNodeProperty: %v", err)
		}
	}
	return g
}

// newNumericBenchEngine builds an engine over the bench graph with the seek
// enabled or disabled, with the bound btree (and numeric companion) on
// (:Person, age).
func newNumericBenchEngine(b *testing.B, n int, disableSeek bool) *cypher.Engine {
	b.Helper()
	g := buildNumericBenchGraph(b, n)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableRangeIndexSeek: disableSeek})
	if _, err := eng.Run(context.Background(),
		`CREATE INDEX FOR (n:Person) ON (n.age) OPTIONS {indexType:'btree'}`, nil); err != nil {
		b.Fatalf("CREATE INDEX: %v", err)
	}
	return eng
}

// runNumericRangeBench drives a selective numeric range query b.N times,
// draining each result so the whole pipeline (seek/scan + residual filter +
// projection) executes.
func runNumericRangeBench(b *testing.B, eng *cypher.Engine) {
	b.Helper()
	ctx := context.Background()
	const q = `MATCH (p:Person) WHERE p.age >= 1000 AND p.age < 1100 RETURN p.age`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.Run(ctx, q, nil)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		var rows int
		for res.Next() {
			rows++
		}
		if err := res.Err(); err != nil {
			b.Fatalf("iter: %v", err)
		}
		if cerr := res.Close(); cerr != nil {
			b.Fatalf("Close: %v", cerr)
		}
		if rows == 0 {
			b.Fatal("expected a non-empty selective range")
		}
	}
}

// BenchmarkNumericRangeSeek_Seek measures the query with the unified numeric
// companion seek active over ~50k nodes.
func BenchmarkNumericRangeSeek_Seek(b *testing.B) {
	const n = 50000
	eng := newNumericBenchEngine(b, n, false)
	runNumericRangeBench(b, eng)
}

// BenchmarkNumericRangeSeek_Scan measures the same query with the seek disabled
// (label scan + residual filter) over the same ~50k nodes.
func BenchmarkNumericRangeSeek_Scan(b *testing.B) {
	const n = 50000
	eng := newNumericBenchEngine(b, n, true)
	runNumericRangeBench(b, eng)
}
