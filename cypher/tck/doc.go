// Package tck implements the openCypher Technology Compatibility Kit (TCK)
// parser-only scenario runner. It reads the vendored Gherkin feature files
// from the embedded [features] directory, extracts every "When executing
// query:" step, and runs each Cypher string through [parser.Parse].
//
// # Scope
//
// The runner covers all 220 feature files (1 615 scenarios total) from the
// openCypher TCK corpus (opencypher/openCypher@main, retrieved 2026-05-20).
// Of these, 523 scenarios are excluded from the pass-rate gate because they
// exercise grammar features not yet supported by the antlr/grammars-v4 grammar
// pinned at commit 284602b. See [skipReason] for the full taxonomy.
//
// The 1 092 remaining scenarios must pass at 100 %. A regression drops the
// pass rate below 100 % and causes [TestTCKParserOnly] to fail, blocking CI.
//
// # Concurrency
//
// [TestTCKParserOnly] is a standard Go test. It may be run under -race without
// additional synchronisation because every scenario invokes [parser.Parse] in
// its own goroutine via t.Run, and parser.Parse is documented as
// concurrency-safe.
//
// # Feature file provenance
//
// Feature files under features/ are vendored from:
//
//	https://github.com/opencypher/openCypher/tree/main/tck/features
//
// Licensed under the Apache License, Version 2.0. See individual file headers.
package tck
