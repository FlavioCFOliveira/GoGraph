package cypher_test

// numeric_equality_seek_bench_test.go — rmp #2169.
//
// Evidence for the acceptance criterion "latency is flat in label population
// where it was previously linear". A numeric equality predicate now seeks the
// float64 btree companion as the closed range [v, v]; before, it scanned the
// whole label and filtered every row.
//
// Both arms run at three label populations spanning 16x, so the SHAPE of each
// curve is the measurement:
//
//	go test -run x -bench BenchmarkNumericEqualitySeek -benchmem -count=6 ./cypher/
//
// Seek/N is roughly constant in N (one index descent plus a residual filter over
// a single candidate); Scan/N grows linearly with N.
//
// # Two projection shapes, because only one of them reaches the seek
//
// The benchmarks come in an _Entity and a _Scalar flavour, and the difference
// between them is itself a finding rather than an accident.
//
//   - RETURN p (entity projection) takes the row-mode Selection build, where the
//     seek rewrite lives. Measured: 5.67 us at N=4000 and 5.77 us at N=16000
//     with the seek, against 814 us and 3.27 ms without — flat against linear,
//     which is what this task set out to deliver.
//   - RETURN p.age (scalar projection) is claimed by the COLUMNAR scan+filter
//     path, which never consults the index seek and scans the whole label.
//     Measured at ~550 us / ~2.24 ms with the seek enabled AND disabled: the
//     rewrite is inert there, not because the predicate is unsupported but
//     because the plan never reaches it.
//
// The columnar recogniser is shape-sensitive in a way that makes this
// inconsistent even for the pre-existing range seek: it claims a single
// comparison but declines an AND of two, so `p.age >= 1000 AND p.age < 1100`
// with the same RETURN p.age DOES seek (see BenchmarkNumericRangeSeek_Seek,
// 36.7 us against 14.0 ms at 50k). The _Scalar benchmarks are retained as the
// standing evidence for that gap; see the backlog item they reference.
//
// Layer: short. Each engine is built once, outside the timed loop and outside
// the b.N retry loop, so neither construction nor the index backfill is
// measured.

import (
	"context"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// equalityBenchPopulations spans 16x so a linear curve is unmistakable against a
// flat one. Every value clears rangeSeekMinLabelPopulation (1024), below which
// the planner always scans.
var equalityBenchPopulations = []int{4000, 16000, 64000}

// newEqualityBenchEngine builds an n-node :Person graph where even i carries the
// integer i and odd i the float i+0.5 — the same mixed-type population the
// #1652 range benchmark uses — with the btree index and its numeric companion
// on (:Person, age). disableSeek selects the scan arm.
//
// The graph is a multigraph purely to avoid the engine's non-multigraph warning,
// which otherwise interleaves with benchmark output; there are no edges, so the
// choice cannot affect the measurement.
func newEqualityBenchEngine(tb testing.TB, n int, disableSeek bool) *cypher.Engine {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Multigraph: true})
	for i := 0; i < n; i++ {
		k := "p" + strconv.Itoa(i)
		if err := g.AddNode(k); err != nil {
			tb.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "Person"); err != nil {
			tb.Fatalf("SetNodeLabel: %v", err)
		}
		var v lpg.PropertyValue
		if i%2 == 0 {
			v = lpg.Int64Value(int64(i))
		} else {
			v = lpg.Float64Value(float64(i) + 0.5)
		}
		if err := g.SetNodeProperty(k, "age", v); err != nil {
			tb.Fatalf("SetNodeProperty: %v", err)
		}
	}
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableRangeIndexSeek: disableSeek})
	if _, err := eng.Run(context.Background(),
		`CREATE INDEX FOR (n:Person) ON (n.age) OPTIONS {indexType:'btree'}`, nil); err != nil {
		tb.Fatalf("CREATE INDEX: %v", err)
	}
	return eng
}

// runNumericEqualityBench drives an equality query whose target sits in the
// middle of the value space, so neither arm benefits from early termination.
// Even targets match exactly one node. projection selects the RETURN item and
// therefore which execution path claims the query — see the file comment.
func runNumericEqualityBench(b *testing.B, eng *cypher.Engine, target int, projection string) {
	b.Helper()
	ctx := context.Background()
	q := `MATCH (p:Person) WHERE p.age = ` + strconv.Itoa(target) + ` RETURN ` + projection
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
		if rows != 1 {
			b.Fatalf("expected exactly 1 row for age = %d, got %d", target, rows)
		}
	}
}

// benchNumericEquality builds each engine once, then measures at each
// population.
func benchNumericEquality(b *testing.B, disableSeek bool, projection string) {
	for _, n := range equalityBenchPopulations {
		eng := newEqualityBenchEngine(b, n, disableSeek)
		target := n / 2
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			runNumericEqualityBench(b, eng, target, projection)
		})
	}
}

// BenchmarkNumericEqualitySeek_EntitySeek measures numeric equality with the
// index access path active on the entity-projection shape — the audit's own
// example, MATCH (a:Person {id: v}) RETURN a. Should be flat in N.
func BenchmarkNumericEqualitySeek_EntitySeek(b *testing.B) {
	benchNumericEquality(b, false, "p")
}

// BenchmarkNumericEqualitySeek_EntityScan is the same shape with the seek
// disabled — the behaviour before rmp #2169. Should be linear in N.
func BenchmarkNumericEqualitySeek_EntityScan(b *testing.B) {
	benchNumericEquality(b, true, "p")
}

// BenchmarkNumericEqualitySeek_ScalarSeek measures the scalar-projection shape with the
// seek enabled. Since #2204 it matches _EntitySeek, not _ScalarScan: the columnar
// recogniser now DECLINES when a covering seek would fire, so the index access path is
// reached whatever the RETURN shape.
//
// It used to match _ScalarScan — 553 us at N=4000 and 2.24 ms at N=16000, identical with
// the seek disabled — because the columnar chain claimed the shape at the Projection
// level before buildOperator could reach the Selection-level rewrite. An index made the
// query SLOWER than not having one, purely as a function of what was projected.
// Measured after the fix: 5.1 us and 4.9 us, i.e. 110x and 463x.
func BenchmarkNumericEqualitySeek_ScalarSeek(b *testing.B) {
	benchNumericEquality(b, false, "p.age")
}

// BenchmarkNumericEqualitySeek_ScalarScan is the scalar-projection shape with
// the seek disabled, for the comparison above.
func BenchmarkNumericEqualitySeek_ScalarScan(b *testing.B) {
	benchNumericEquality(b, true, "p.age")
}
