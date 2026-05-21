// Package tck records the conformance evolution of the GoGraph Cypher engine
// against the openCypher Technology Compatibility Kit.
//
// # Conformance History
//
// The pass rate below is measured at the parser level (grammar + AST round-trip).
// Execution-level conformance is tracked separately in docs/tck/DIVERGENCES.md.
//
//	Sprint 26: ~70%  — baseline after initial grammar implementation
//	Sprint 27: ~72%  — write-clause parser support
//	Sprint 28: ~74%  — expression improvements, MATCH patterns
//	Sprint 29: 76.5% → 90.7% — normalizeSingleQuotes pre-processor resolved 579
//	           single-quote-string scenarios; DDL/procedure scenarios added
//	Sprint 30: 90.7% — Bolt server; no parser changes
//	Sprint 31: 90.7% — godog execution runner added; parser rate unchanged;
//	           execution-level rate baseline = 10.4% (407/3897 scenarios)
//
// To improve execution-level conformance, see docs/tck/DIVERGENCES.md Category 3
// and Category 4 for the roadmap of engine enhancements needed.
package tck
