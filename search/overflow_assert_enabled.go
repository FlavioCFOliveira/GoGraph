//go:build gograph_debug

package search

import "fmt"

// assertNoRelaxOverflow panics when the relaxation result cand = prev +
// weight has overflowed the Weight type W, which for the bounded integer
// types manifests as a wrapped value. It is compiled only under the
// gograph_debug build tag (development and CI); released binaries link
// the no-op in overflow_assert_disabled.go and pay no overhead.
//
// Overflow oracle. For an exact (non-wrapping) addition the sum lands on
// the side of prev dictated by the sign of weight:
//
//   - weight > 0  =>  cand > prev   (a smaller cand means positive overflow);
//   - weight < 0  =>  cand < prev   (a larger  cand means negative overflow);
//   - weight == 0 =>  cand == prev.
//
// The predicate (weight > 0 && cand < prev) || (weight < 0 && cand >
// prev) is therefore exactly the integer-wraparound signature. It is
// weight-type agnostic: for floating-point W it never fires on finite
// inputs (NaN/Inf are rejected at the public boundary by anyFloatInvalid
// before any relaxation runs), so the check is meaningful only where it
// can actually be violated — the integer Weight types whose cumulative
// distance the caller must keep in range.
//
// A firing assertion signals that the caller violated the documented
// precondition that the longest relevant path's cumulative weight fits
// W (see [Dijkstra], [BellmanFord], [JohnsonAPSP], [FloydWarshall]).
// Per the module's failure-handling contract this is a programmer error
// and must surface immediately rather than be papered over with a
// silently wrong shortest path.
func assertNoRelaxOverflow[W Weight](prev, weight, cand W) {
	var zero W
	if (weight > zero && cand < prev) || (weight < zero && cand > prev) {
		panic(fmt.Sprintf(
			"search: integer distance overflow during relaxation: prev=%v + weight=%v wrapped to cand=%v; "+
				"the cumulative path weight exceeds the Weight type's range (see the algorithm's godoc precondition)",
			prev, weight, cand,
		))
	}
}
