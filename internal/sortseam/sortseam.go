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

// keyHoistDisabled is the control for the SECOND half of the sort seam. False —
// the zero value, and therefore the production setting — lets the translator
// project a `var.prop` ORDER BY key into its own hidden column (#2662), so the
// sort resolves it by schema lookup and the entity it is read from never has to
// survive the projection. True restores the pre-#2662 behaviour, in which the
// projection carries the whole entity and the sort compiles an expression
// evaluator over it.
//
// It lives here rather than in cypher/ir for the reason the package comment
// gives for the first control: it is WRITTEN by tests that must build an Engine
// (package cypher, and the bench/audit352 harness) and READ by the translator,
// and neither can reach the other's unexported identifiers.
//
// # Caching
//
// Unlike [KeyDecorationDisabled], which the sort operators read per execution,
// this control is read at TRANSLATE time — and a translated plan is cached per
// [cypher.Engine]. Flipping it therefore has no effect on a query whose plan the
// engine has already cached. A differential caller must give each arm its OWN
// engine, built and warmed with the control already set to that arm's value.
var keyHoistDisabled atomic.Bool

// KeyHoistDisabled reports whether the translator must keep the pre-#2662 entity
// passthrough instead of projecting the ORDER BY key into its own column. It is
// read once per translated ORDER BY term, never per row.
func KeyHoistDisabled() bool { return keyHoistDisabled.Load() }

// SetKeyHoistDisabled sets the control and returns a function that restores the
// value it replaced. Call the restore function with defer.
//
// The same whole-process discipline [SetKeyDecorationDisabled] documents applies,
// with the added caching caveat on [keyHoistDisabled]: set it BEFORE the engine
// that must observe it translates the query.
func SetKeyHoistDisabled(disabled bool) (restore func()) {
	prev := keyHoistDisabled.Swap(disabled)
	return func() { keyHoistDisabled.Store(prev) }
}
