//go:build soak || nightly

package expandinto_test

// expandinto_soak_bench_test.go — the high-degree tail of the sweep (#2152).
//
// Layer: soak. These degrees are here rather than in the short layer for one reason:
// the DISABLED arm's cost is Θ(n·d²), so at 20 000 nodes it runs for seconds per
// operation — out-degree 64 alone measured 2.98 s — and 128 and 256 would blow the
// short layer's 60 s per-package budget several times over. The enabled arm is cheap at
// every degree; it is the baseline that is expensive, which is the point being measured.
//
//	go test -run='^$' -bench=. -benchmem -count=6 -tags=soak ./bench/expandinto/
//
// The exponent this tail supports is the whole claim, so it is recorded rather than
// dropped: the short layer's 8→64 window understates the win, because the seek's
// advantage grows with degree.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bench/expandinto"
)

// BenchmarkClosingHopHighDegree extends BenchmarkClosingHop to the degrees the short
// layer cannot afford.
func BenchmarkClosingHopHighDegree(b *testing.B) {
	benchClosing(b, expandinto.ClosingQuery, []int{128, 256}, false)
}

// BenchmarkSymmetricAnchorSwapHighDegree extends the swap sweep to a hub degree where
// the written plan's cost is unmistakable.
func BenchmarkSymmetricAnchorSwapHighDegree(b *testing.B) {
	benchSymmetricSwap(b, []int{200000})
}
