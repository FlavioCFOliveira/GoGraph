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
package tck
