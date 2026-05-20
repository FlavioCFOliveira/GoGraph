// Package parser translates the ANTLR4-generated Cypher parse tree into the
// typed AST defined in gograph/cypher/ast. The generated sources (under
// cypher/parser/gen/) are produced by the ANTLR4 tool and must not be
// hand-edited.
//
// Build the parser from the grammar with:
//
//	go generate ./cypher/parser/...
//
// # Error Recovery Contract
//
// The parser uses ANTLR's default single-token insertion/deletion strategy
// (antlr.DefaultErrorStrategy). Under this strategy:
//
//   - On a syntax error the parser attempts to recover by either inserting a
//     missing single token or deleting the current token. This allows parsing
//     to continue past isolated mistakes and collect further errors.
//
//   - A maximum of [maxParseErrors] errors (currently 5) are collected per
//     parse. Once the cap is reached, additional errors are silently dropped.
//     This prevents cascading error floods on pathological inputs where a
//     single structural mistake causes the parser to mis-identify every
//     subsequent token as erroneous.
//
//   - When at least one error is present the parse tree may be partial: some
//     subtrees may have been constructed using the inserted/deleted tokens
//     produced by error recovery. Callers must not rely on the AST being
//     semantically correct when errors are returned.
//
//   - [Parse] returns only the first error. [ParseStrict] returns all
//     collected errors (up to the cap) and is intended for tooling such as
//     editors and linters that benefit from seeing multiple errors at once.
//
// # Concurrency
//
// [Parse] and [ParseStrict] are safe to call concurrently. Each invocation
// creates independent lexer, parser, and error-listener instances; no state
// is shared between calls.
package parser
