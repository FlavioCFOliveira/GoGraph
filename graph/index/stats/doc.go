// Package stats holds the best-effort, approximate planner statistics that back
// the Cypher optimiser's cardinality estimates for single-column predicates
// (design docs/statistics-design.md, tasks #2097 / #2098). It complements the
// EXACT population counts of the sibling count-store (graph/index/count): every
// selectivity is S = C/N with an exact denominator N (N(label) / E(relType)),
// so only the numerator — an in-range row count, or a number of distinct values
// — is ever approximate, and that numerator is what this package estimates.
//
// # Structures
//
//   - [HLL] — a HyperLogLog++ distinct-value (NDV) estimator over 64-bit hashes,
//     m = 2^12 = 4096 registers, 6-bit packed, with a sparse low-cardinality
//     representation and a linear-counting small-range correction. Relative
//     standard error 1.04/√m ≈ 1.625%.
//   - [Histogram] — an equi-depth histogram (B = 256 buckets) whose worst-case
//     absolute selectivity error is the distribution-free 1/B ≈ 0.39% bound
//     (Piatetsky-Shapiro & Connell, SIGMOD'84), with heavy (most-common) values
//     isolated into singleton buckets so the bound survives skew (the MaxDiff
//     mechanism, Poosala et al. SIGMOD'96).
//   - [MCVList] — the EXACT top-k (k = 32) most-common values, selected by a
//     bounded min-heap over the rebuild scan. Exact, not Count-Min: a Count-Min
//     sketch over-estimates, which is the unsafe direction for a no-regression
//     safety gate.
//   - [Stats] — the per-(label, property) bundle of the three estimators plus a
//     generation stamp (g0, N0) and the atomic staleness counters.
//   - [Collector] — the lock-free-read registry mapping (labelID, propID) to
//     [Stats], published by an atomic snapshot swap.
//
// # Maintenance model
//
// The estimators are built by a caller-driven full scan (never a background
// goroutine); a fresh snapshot is published atomically with [Collector.Publish].
// Absence or staleness is harmless: a consumer that finds no fresh statistic
// simply falls back to the exact-count plan. The only write-path cost the design
// permits is an O(1) atomic per-(label, property) dirty-write counter (Δ), bumped
// through [Collector.RecordWrite]; everything else is rebuilt off the write path.
// HLL cannot delete (a register maximum only rises), so deletes over-estimate NDV
// (the unsafe direction) and are tracked separately ([Stats.RecordDelete]) to
// force a rebuild once they exceed a small tolerance of N.
//
// # Value-domain neutrality
//
// The package is generic over the value type T and never imports the Cypher
// value model: callers inject an orderability comparator (for [Histogram]) and a
// hash / equivalence pair (for [MCVList] and [HLL]). The Cypher layer instantiates
// everything with expr.Value, expr.Compare (the CIP2016-06-14 orderability
// comparator, which routes cross-type Integer/Float boundaries through the exact
// cmpInt64Float64), and expr.EquivalentHash / expr.Equivalent, keeping the
// openCypher value semantics — NaN exclusion, −0.0 == +0.0, one histogram per
// comparable value-domain — where they belong.
//
// # Concurrency contract
//
// A [Collector] is safe for concurrent use: reads ([Collector.Lookup],
// [Collector.TrackedLabelsForProp], [Collector.Tracking]) are lock-free (an
// atomic snapshot-pointer load over an immutable map), the per-[Stats] staleness
// counters are atomics, and [Collector.Publish] swaps a freshly-built snapshot in
// one atomic store. A [HLL], [Histogram], or [MCVList] is immutable once built
// and is safe for concurrent reads; building one (Insert / BuildEquiDepth /
// BuildTopK) is single-threaded and must complete before the value is published.
// The package spawns no goroutines.
package stats
