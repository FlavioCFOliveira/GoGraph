package expandinto_test

// expandinto_bench_test.go — short-layer benchmarks for the bound-destination seek
// (#2149) and the symmetric anchor swap (#2150), measured under #2152.
//
// Layer: short. The degrees that exceed the short layer's time budget with the seek
// DISABLED (128 and 256, where the disabled arm runs for seconds per op) are gated to
// the soak layer in expandinto_soak_bench_test.go.
//
//	go test -run='^$' -bench=. -benchmem -count=6 ./bench/expandinto/
//	go test -run='^$' -bench=. -benchmem -count=6 -tags=soak ./bench/expandinto/
//
// Every benchmark pairs the two access paths in ONE process, so the comparison is
// immune to the back-to-back cross-commit artefact documented in the package comment.

import (
	"context"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bench/expandinto"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// benchNodes is held constant across the sweep so out-degree is the only variable.
// 20 000 matches the audit's §2.3 harness, keeping this comparable to the figure the
// sprint was opened on.
const benchNodes = 20000

// shortDegrees are the degrees whose DISABLED arm still fits the short layer.
var shortDegrees = []int{8, 32, 64}

// drain executes q to completion once, failing the benchmark on any error.
func drain(b *testing.B, eng *cypher.Engine, q string) {
	b.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		b.Fatal(err)
	}
	for res.Next() { //nolint:revive // intentional full drain
	}
	if err := res.Err(); err != nil {
		b.Fatal(err)
	}
	_ = res.Close()
}

// engineFor builds an engine with the seek enabled or disabled and WARMS IT, so the
// CSR pair cache is populated before the timer starts.
//
// The warm-up is not cosmetic. Without it the first timed iteration also builds and
// caches the forward and reverse CSRs, which at out-degree 64 reported 319 MB/op
// against 61.8 MB/op for every later iteration — an amortisation artefact that would
// be attributed to the access path under test.
func engineFor(b *testing.B, g *lpg.Graph[string, float64], q string, seek bool) *cypher.Engine {
	b.Helper()
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableExpandIntoSeek: !seek})
	drain(b, eng, q)
	return eng
}

// benchClosing runs q over a ring fixture at each degree, with the seek on and off.
func benchClosing(b *testing.B, q string, degrees []int, mutual bool) {
	benchClosingAt(b, benchNodes, q, degrees, mutual)
}

// benchClosingAt is benchClosing with an explicit node count, for the shapes whose
// cost is not bounded by the closing hop.
func benchClosingAt(b *testing.B, nodes int, q string, degrees []int, mutual bool) {
	for _, d := range degrees {
		g, err := expandinto.SeedRing(nodes, d, mutual)
		if err != nil {
			b.Fatalf("SeedRing(%d, %d): %v", nodes, d, err)
		}
		for _, seek := range []bool{true, false} {
			arm := "seek"
			if !seek {
				arm = "filter"
			}
			b.Run("degree"+strconv.Itoa(d)+"/"+arm, func(b *testing.B) {
				eng := engineFor(b, g, q, seek)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					drain(b, eng, q)
				}
			})
		}
	}
}

// BenchmarkClosingHop is the headline: the audit's §2.3 2-cycle shape, whose closing
// hop the seek turns from a Θ(d) walk into an O(log d + r) probe. mutual=false
// reproduces the audit's own fixture, where the closing hop matches nothing and the
// enumeration is pure waste.
func BenchmarkClosingHop(b *testing.B) {
	benchClosing(b, expandinto.ClosingQuery, shortDegrees, false)
}

// BenchmarkClosingHopWithMatches is the same shape over a fixture where 2-cycles
// genuinely exist, so the operator also emits rows. It separates "skipped work" from
// "work that produced an answer".
func BenchmarkClosingHopWithMatches(b *testing.B) {
	benchClosing(b, expandinto.ClosingQuery, shortDegrees, true)
}

// openFixtureNodes is far smaller than [benchNodes] because the shapes that EMIT
// Θ(n·d²) rows are sized by their own output, not by the closing hop.
//
// This is a harness correctness point, not a preference. Run first at 20 000 nodes,
// BenchmarkOpenControl at out-degree 64 emitted ~82 M rows and cost 36 SECONDS per
// operation — a benchmark nobody will run, measuring row construction rather than the
// access path under test. The control's job is only to show the flag is INERT where no
// destination is bound, which a small fixture establishes just as well.
const openFixtureNodes = 1500

// BenchmarkTriangle records the LIMIT of the change rather than a win. The middle hop
// is open, so Θ(n·d²) intermediate rows are materialised however the closing hop runs,
// and the end-to-end exponent stays near 2. Degrees are smaller because the cost is
// cubic in d before the seek applies at all.
func BenchmarkTriangle(b *testing.B) {
	benchClosingAt(b, openFixtureNodes, expandinto.TriangleQuery, []int{4, 8, 16}, true)
}

// BenchmarkOpenControl is the paired control: the same two hops with a free
// destination, where no bound-destination path exists. Both arms must be within noise
// of each other — a difference here would mean the flag is changing something it has no
// business changing.
func BenchmarkOpenControl(b *testing.B) {
	benchClosingAt(b, openFixtureNodes, expandinto.OpenControlQuery, shortDegrees, true)
}

// BenchmarkSymmetricAnchorSwap measures the reverse-introducing single-edge swap
// (#2150) against the OUT-only baseline it replaces, sweeping the hub's out-degree.
//
// The written plan walks every one of the hub's out-edges; the swapped plan scans the
// 50 Leaf nodes and walks the single in-edge. So the win grows with the hub's degree,
// which is why this is a sweep and not a point.
func BenchmarkSymmetricAnchorSwap(b *testing.B) {
	benchSymmetricSwap(b, []int{1601, 40000})
}

// benchSymmetricSwap is shared with the soak-layer high-degree variant.
func benchSymmetricSwap(b *testing.B, hubOuts []int) {
	for _, hubOut := range hubOuts {
		g, err := expandinto.SeedReverseHub(hubOut, 50)
		if err != nil {
			b.Fatalf("SeedReverseHub(%d): %v", hubOut, err)
		}
		for _, swap := range []bool{true, false} {
			arm := "symmetric"
			if !swap {
				arm = "outonly"
			}
			b.Run("hubOut"+strconv.Itoa(hubOut)+"/"+arm, func(b *testing.B) {
				eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableAnchorSwap: !swap})
				drain(b, eng, expandinto.SwapQuery) // warm the CSR pair cache
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					drain(b, eng, expandinto.SwapQuery)
				}
			})
		}
	}
}
