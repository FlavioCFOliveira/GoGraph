// Package sortseam carries the one diagnostic control that forces the Cypher
// sort operators back onto their LEGACY per-comparison sort-key evaluation path.
//
// # Why this package exists
//
// The control has to be READ by github.com/FlavioCFOliveira/GoGraph/cypher/exec,
// where the Sort and Top operators live, and WRITTEN by
// github.com/FlavioCFOliveira/GoGraph/cypher, because only a test that can build
// an Engine can run a real query — and a real query is what both consumers of
// the control need:
//
//   - the differential test that proves the decorated sort returns byte-identical
//     rows, in byte-identical tie order, to the legacy sort; and
//   - the interleaved single-binary A/B the profiler runs over the reproduction
//     query, which must flip arms inside ONE process rather than across two
//     builds.
//
// Neither package can reach the other's unexported identifiers, and adding an
// exported knob to cypher/exec would enlarge the module's PUBLIC API for a
// control that exists only to serve tests. Go's internal-package rule makes
// everything below unimportable outside this module, so the identifiers are
// exported for those two packages' benefit without ever becoming public API.
//
// # Scope and concurrency
//
// The control is process-global, exactly like the diagnostic counters it is used
// alongside (exec.ExpandIntoSeekCount and the planner's build counters). It is
// safe to read and write concurrently, but a test that flips it is choosing an
// execution path for the WHOLE process, so such a test must not call
// t.Parallel() and must restore the previous value before returning. Use the
// restore function [SetKeyDecorationDisabled] returns for that.
//
// Nothing in the module's normal operation writes it: the decorated path is
// always on in production.
package sortseam

import "sync/atomic"

// keyDecorationDisabled is the control. False — the zero value, and therefore
// the production setting — selects decorate-sort-undecorate: each ORDER BY key
// is materialised once per row and the comparator reads only the precomputed
// values. True restores the pre-#2652 behaviour, in which the comparator itself
// evaluates both operands of every comparison.
var keyDecorationDisabled atomic.Bool

// KeyDecorationDisabled reports whether the sort operators must use the legacy
// per-comparison sort-key evaluation path. It is read once per blocking sort
// phase, never per row and never per comparison.
func KeyDecorationDisabled() bool { return keyDecorationDisabled.Load() }

// SetKeyDecorationDisabled sets the control and returns a function that restores
// the value it replaced. Call the restore function with defer.
//
// Because the control is process-global, a caller must hold the whole process
// for the span between the two calls: no t.Parallel(), and no concurrent query
// whose arm the caller is not choosing deliberately.
func SetKeyDecorationDisabled(disabled bool) (restore func()) {
	prev := keyDecorationDisabled.Swap(disabled)
	return func() { keyDecorationDisabled.Store(prev) }
}
