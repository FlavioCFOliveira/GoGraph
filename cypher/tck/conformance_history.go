// Package tck records the conformance evolution of the GoGraph Cypher engine
// against the openCypher Technology Compatibility Kit.
//
// # Conformance History
//
// Parser-level pass rate (grammar + AST round-trip):
//
//	Sprint 26: ~70%  — baseline after initial grammar implementation
//	Sprint 27: ~72%  — write-clause parser support
//	Sprint 28: ~74%  — expression improvements, MATCH patterns
//	Sprint 29: 76.5% → 90.7% — normalizeSingleQuotes pre-processor resolved 579
//	           single-quote-string scenarios; DDL/procedure scenarios added
//	Sprint 30: 90.7% — Bolt server; no parser changes
//	Sprint 31: 90.7% — godog execution runner added; parser rate unchanged;
//	           execution-level rate baseline = 10.4% (407/3897 scenarios)
//	Sprint 43: 100.0% — task #402 closed the last grammar-gap-literal sub-class;
//	           parser is fully green (3897/3897)
//
// Execution-level pass rate (full godog runner, tckExecutionBaseline gate):
//
//	Sprint 31: 10.4% (407/3897) — baseline
//	Sprint 37: 24.8% (968/3897)
//	Sprint 42: 29.6% (1152/3897)
//	Sprint 46: 39.4% (1536/3897)
//	Sprints 58–64 (rounds 22–64, 2026-05-28/29): 100.0% (3897/3897) — FULLY GREEN
//	  Key uplifts: error-step regex (R58), VLE cross-pattern no-repeat-rel (R59+R61),
//	  PatternComprehension + percentile guard (R60), CREATE-multiplicity counter (R62),
//	  per-CREATE-instance edge labels + multigraph adjlist (R63), named-path
//	  leading-hop reconstruction (R64).
//
// The enforced gate is const tckExecutionBaseline = 3897 in runner_test.go.
// Any PR that lowers the passing count is rejected by CI.
// See docs/tck/DIVERGENCES.md for the full audit trail.
//
// # Gate scope: what "3897/3897" actually verifies
//
// The 3897 baseline is a full row/value comparator: every scenario's result
// set is checked exactly — multiset or ordered sequence, every column,
// including node/relationship/list/map/temporal/null values — against the
// scenario's declared expectation. A scenario asserting an error passes when
// ANY error occurs, not necessarily the openCypher-specified error TYPE (see
// assertError/assertSyntaxError in errors_test.go); exact error-type fidelity
// is tracked by a separate, narrower ratchet, tckErrorFidelityBaseline (122 of
// ~695 error assertions as of 2026-06-13), in error_fidelity_test.go. A
// scenario's declared side effects (`+nodes N`, `+relationships N`, etc.) are
// checked as a lower bound, not an exact count, and only for nodes/
// relationships — `+properties`/`-properties`/`+labels`/`-labels` and the
// `no side effects` step are not verified at all (see sideEffectsTable and
// noSideEffects in compare_test.go). None of this contradicts the "100%
// openCypher TCK compliant" claim for query RESULTS, which is what the
// scenario's `Then` clause and this gate are about — it scopes what the
// number does not additionally guarantee (2026-07-02 production-readiness
// audit finding F3/INFO).
package tck
