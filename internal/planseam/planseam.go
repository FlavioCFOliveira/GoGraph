// Package planseam carries the Cypher planner's diagnostic build counters — the
// seams a test reads to prove that the physical operator it means to measure is
// the one the engine actually built.
//
// # Why this package exists
//
// The counters are WRITTEN by github.com/FlavioCFOliveira/GoGraph/cypher, whose
// planner is the only thing that can bump them, and READ by two kinds of
// consumer:
//
//   - the differential tests inside package cypher, which assert a structural
//     trigger fired (or, under a guard, did not); and
//   - the out-of-package measurement harnesses under bench/, which must prove a
//     benchmark exercises the operator its finding is attributed to before any
//     number it produces means anything.
//
// The second consumer cannot reach an unexported variable in package cypher, and
// exporting one from package cypher would enlarge the module's PUBLIC API for a
// control that exists only to serve tests. Go's internal-package rule makes
// everything below unimportable outside this module, so the identifiers are
// exported for those consumers' benefit without ever becoming public API. This
// is the same reasoning, and the same shape, as
// github.com/FlavioCFOliveira/GoGraph/internal/sortseam.
//
// # Scope and concurrency
//
// The counters are process-global and monotonic: they count builds across every
// engine and every goroutine in the process. They are safe to read and write
// concurrently. A reader snapshots one before an operation and compares after;
// a reader that needs the delta to be attributable to its own query must not run
// concurrently with another query that could bump the same counter.
//
// Nothing in the module's normal operation reads them; the planner's behaviour
// does not depend on their value.
package planseam

import "sync/atomic"

// ParallelScanProjectBuilds counts how many times the planner has emitted the
// morsel-parallel fused scan→filter→project leaf (exec.ParallelScanProject, #1682)
// in place of the serial AllNodesScan → Filter → Projection pipeline.
//
// It is the seam that distinguishes the two routes a pure-property projection can
// take: this operator, or the columnar filter/project chain (#2065). A benchmark
// that attributes a cost to ParallelScanProject and never checks this counter is
// measuring an operator it has not shown is running.
var ParallelScanProjectBuilds atomic.Uint64
