//go:build !gograph_debug

package search

// assertNoRelaxOverflow is the production (non-debug) no-op form of the
// integer-distance overflow assertion. It is compiled into every binary
// that omits the gograph_debug build tag — which is every released
// binary — and the compiler elides its empty body entirely, so the
// relaxation hot loops of Bellman-Ford and Johnson pay zero overhead.
//
// The active implementation (which panics when a relaxation result
// lands on the wrong side of its predecessor, the signature of integer
// wraparound) lives in overflow_assert_enabled.go and is compiled only
// under -tags gograph_debug.
//
// Rationale. Detecting cumulative-distance overflow on every relaxation
// in production would slow the inner loop for a condition that is a
// caller precondition violation, not a runtime input error (see the
// godoc on [Dijkstra], [BellmanFord], [JohnsonAPSP] and
// [FloydWarshall]). The build-tag split keeps the check available for
// development and CI without taxing the released hot path, mirroring the
// crash-injection hook in internal/crashpoint.
func assertNoRelaxOverflow[W Weight](_, _, _ W) {}
