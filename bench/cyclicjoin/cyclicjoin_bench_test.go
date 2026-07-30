package cyclicjoin_test

// cyclicjoin_bench_test.go — short-layer benchmarks for the fused cyclic expand
// (rmp #2157, measured under #2159).
//
//	go test -run='^$' -bench=. -benchmem -count=6 ./bench/cyclicjoin/
//	go test -run='^$' -bench=. -benchmem -count=6 -tags=soak ./bench/cyclicjoin/
//
// Every benchmark pairs the fused arm against the two-Expand arm in ONE process,
// toggled by EngineOptions — see the package comment for why a cross-commit A/B is
// inadmissible on this machine.
//
// Sizes here are chosen to stay inside the short layer's 60 s-per-package budget;
// the decade-scale sweep the task asks for lives in cyclicjoin_soak_bench_test.go.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bench/cyclicjoin"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// drain executes q to completion once, failing the benchmark on any error.
func drain(tb testing.TB, eng *cypher.Engine, q string) {
	tb.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		tb.Fatal(err)
	}
	for res.Next() { //nolint:revive // intentional full drain
	}
	if err := res.Err(); err != nil {
		tb.Fatal(err)
	}
	_ = res.Close()
}

// engineFor builds an engine with the fusion on or off and WARMS it, so the CSR
// pair cache is populated before the timer starts.
//
// The warm-up is not cosmetic: an unwarmed first iteration also builds and caches
// the forward and reverse CSRs, an amortisation artefact that would be attributed
// to the access path under test (bench/expandinto documents the same trap, where it
// reported 319 MB/op against 61.8 MB/op for every later iteration).
func engineFor(tb testing.TB, g *lpg.Graph[string, float64], q string, fused bool) *cypher.Engine {
	tb.Helper()
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{EnableCyclicIntersect: fused})
	drain(tb, eng, q)
	return eng
}

// armName renders the toggle for the benchmark sub-name.
func armName(fused bool) string {
	if fused {
		return "fused"
	}
	return "twoexpand"
}

// benchBothArms runs q on g with the fusion off and on, asserting first that the two
// arms AGREE on the row count — a benchmark that measures two different answers is
// worse than no benchmark.
func benchBothArms(b *testing.B, g *lpg.Graph[string, float64], q, label string) {
	b.Helper()
	assertArmsAgree(b, g, q)
	for _, fused := range []bool{false, true} {
		eng := engineFor(b, g, q, fused)
		b.Run(label+"/"+armName(fused), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				drain(b, eng, q)
			}
		})
	}
}

// assertArmsAgree fails the benchmark unless both arms return the same single count,
// so a performance number can never be reported for divergent behaviour.
func assertArmsAgree(tb testing.TB, g *lpg.Graph[string, float64], q string) {
	tb.Helper()
	count := func(fused bool) string {
		eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{EnableCyclicIntersect: fused})
		res, err := eng.Run(context.Background(), q, nil)
		if err != nil {
			tb.Fatal(err)
		}
		defer func() { _ = res.Close() }()
		out := ""
		for res.Next() {
			if v := res.ValueAt(0); v != nil {
				out += v.String() + ";"
			}
		}
		if err := res.Err(); err != nil {
			tb.Fatal(err)
		}
		return out
	}
	if off, on := count(false), count(true); off != on {
		tb.Fatalf("the two arms DISAGREE on %q: twoexpand=%q fused=%q", q, off, on)
	}
}

// shortNodes keeps the fixtures inside the short layer's budget.
const shortNodes = 4000

// BenchmarkCyclic_Triangle_Uniform is the headline shape on the honest floor: a
// uniform-degree fixture, where the SPIKE proved the two plans' work terms are
// EXACTLY equal, so anything measured here is constant-factor and materialisation
// only, with no skew advantage.
func BenchmarkCyclic_Triangle_Uniform(b *testing.B) {
	for _, degree := range []int{4, 16, 64} {
		g, err := cyclicjoin.SeedUniform(shortNodes, degree)
		if err != nil {
			b.Fatalf("SeedUniform(%d, %d): %v", shortNodes, degree, err)
		}
		benchBothArms(b, g, cyclicjoin.TriangleQuery, "d="+itoaBench(degree))
	}
}

// BenchmarkCyclic_Triangle_SmallDegree covers the residual risk SPIKE #2155
// recorded as UNMEASURED: at degree 1–2 the merge's setup could plausibly lose to a
// trivial scan. If a crossover exists the recognition predicate needs a degree
// floor, and this is where that would show.
func BenchmarkCyclic_Triangle_SmallDegree(b *testing.B) {
	for _, degree := range []int{1, 2, 3} {
		g, err := cyclicjoin.SeedUniform(shortNodes, degree)
		if err != nil {
			b.Fatalf("SeedUniform(%d, %d): %v", shortNodes, degree, err)
		}
		benchBothArms(b, g, cyclicjoin.TriangleQuery, "d="+itoaBench(degree))
	}
}

// BenchmarkCyclic_Triangle_PowerLaw is the skewed fixture — the only regime in
// which the SPIKE measured an asymptotic separation (1.112 against 1.008).
func BenchmarkCyclic_Triangle_PowerLaw(b *testing.B) {
	g, err := cyclicjoin.SeedPowerLaw(shortNodes, 8, 0.9, 315201)
	if err != nil {
		b.Fatalf("SeedPowerLaw: %v", err)
	}
	benchBothArms(b, g, cyclicjoin.TriangleQuery, "powerlaw")
}

// BenchmarkCyclic_TwoCycle measures the motivating audit's own §2.3 shape, which
// fuses although nothing in the design anticipated it.
func BenchmarkCyclic_TwoCycle(b *testing.B) {
	g, err := cyclicjoin.SeedUniform(shortNodes, 16)
	if err != nil {
		b.Fatalf("SeedUniform: %v", err)
	}
	benchBothArms(b, g, cyclicjoin.ClosingQuery, "d=16")
}

// BenchmarkCyclic_Square records that a longer cycle adds open hops rather than
// intersections, so its gain should be smaller than the triangle's rather than
// larger.
func BenchmarkCyclic_Square(b *testing.B) {
	g, err := cyclicjoin.SeedUniform(shortNodes, 8)
	if err != nil {
		b.Fatalf("SeedUniform: %v", err)
	}
	benchBothArms(b, g, cyclicjoin.SquareQuery, "d=8")
}

// BenchmarkCyclic_NonQualifying proves the recognition predicate leaves declined
// shapes untouched. Both arms run the SAME plan here, so any difference is machine
// noise and the expected result is statistical parity — which is exactly what makes
// these two rows the control for every row above.
func BenchmarkCyclic_NonQualifying(b *testing.B) {
	g, err := cyclicjoin.SeedUniform(shortNodes, 16)
	if err != nil {
		b.Fatalf("SeedUniform: %v", err)
	}
	benchBothArms(b, g, cyclicjoin.LabelledTriangleQuery, "labelled")
	benchBothArms(b, g, cyclicjoin.AcyclicQuery, "acyclic")
}

func itoaBench(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
