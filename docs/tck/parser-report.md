# TCK Parser-Only Report

**Date:** 2026-05-22  
**Corpus:** openCypher TCK — opencypher/openCypher@main  
**Grammar:** antlr/grammars-v4, commit 284602b (BSD-3)  
**Runner:** `gograph/cypher/tck`, test `TestTCKParserOnly`

---

## Summary

| Metric | Value |
|---|---|
| Total TCK scenarios (post Scenario Outline expansion) | 3 897 |
| Scenarios run against `parser.Parse` | **3 897** |
| Scenarios skipped at the parser gate | 0 |
| Pass rate on run scenarios | **100.0 %** |

Task #402 (Sprint 43) closed the last residual `grammar-gap-literal`
sub-classes. The parser tier is now at full conformance with the
openCypher TCK. See `docs/tck/DIVERGENCES.md` Category 1 for the
historical record of every skip reason that ever existed and the
mechanism that closed it.

This document focuses on **how the parser is assembled**: the
pre-processor pipeline that lives in `cypher/parser/normalize.go`, the
visitor-level validation that lives in `cypher/parser/visitor.go`, and
the post-generation patches that must be reapplied after every ANTLR
regeneration.

---

## Reproducing

```bash
# Run the pass-rate gate (fails if any run scenario regresses):
go test -run TestTCKParserOnly ./cypher/tck/...

# View per-file pass/skip counts:
go test -v -run TestTCKParserOnly ./cypher/tck/...

# View skip-reason inventory (should show only "(run)" after task #402):
go test -v -run TestTCKParserOnlySkipCoverage ./cypher/tck/...

# Run with race detector (required in CI):
go test -race -run TestTCKParserOnly ./cypher/tck/...
```

---

## Pre-lex validation (`validateUnicodeEscapes`)

Before any pre-processor runs, `parser.Parse` calls
`validateUnicodeEscapes` to scan the raw query for malformed `\u` escapes
inside any string literal. The openCypher specification requires every
`\u` (case-insensitive) to be followed by at least one further `u` and
then exactly four hexadecimal digits. The validator returns a
`*ParseError` pinpointing the offending position; if no malformation is
found, the pipeline continues.

This pass runs before `normalizeSingleQuotes` so that the rewriter does
not silently hide a malformed escape inside a benign-looking
double-quoted form (which the ANTLR lexer would then accept by routing
the broken bytes through its `ERRCHAR -> channel(HIDDEN)` rule).

---

## Pre-processor pipeline (`cypher/parser/normalize.go`)

`parser.Parse` runs the input string through the following ordered
normalisers before lexing. Each one is a byte-level scanner that
respects string literals, backtick identifiers, and both line and block
comments. Each has a fast-path early return when its target byte is
absent from the input.

| Order | Pre-processor | Resolves |
|---:|---|---|
| 1 | `normalizeSingleQuotes` | Single-quoted string literals (`'…'` → `"…"`). |
| 2 | `normalizeDoubleNot` | `NOT NOT x` → `x`, `NOT NOT NOT x` → `NOT x`. |
| 3 | `normalizeCallNoParen` | `CALL proc YIELD …` → `CALL proc() YIELD …`. |
| 4 | `normalizeNegHexOct` | `-0x1A`, `-0o777` → signed-decimal form. |
| 5 | `normalizeFloatExpZeroPad` | `2E-01` → `2E-1`, `5e-001` → `5e-1` (strips leading-zero pad from signed exponents). |
| 6 | `normalizeArithmeticMinus` | `n-1` → `n - 1` (so the lexer cannot consume `-1` as a single DIGIT). |
| 7 | `normalizeVarlenDotDot` | `[..M]`, `[N..]`, `[..]` → `[*..M]`, `[*N..]`, `[*..]`. |
| 8 | `normalizeVarlenBounds` | `[*N..M]` → `[*-N..-M]` (negate to force DIGIT tokenisation). |
| 9 | `normalizeZeroDotFloat` | `0.5` → `.5`. |
| 10 | `normalizeLeadingDotFloat` | `.0`, `.05`, `.00123` → `0.0`, `0.05`, `0.00123`. |

`ParseStrict` runs a subset of the pipeline that covers the same
syntactic constructs (without the more aggressive lex-only rewrites).

---

## Visitor-level validation (`cypher/parser/visitor.go`)

A few openCypher rules are enforced after the parse tree is built rather
than at the grammar level. Each is a small, contained check that returns
a `*SemaError` when the rule is violated. Because the parse-time error
contract is satisfied (the visitor's `SemaError` is returned from
`parser.Parse` exactly like a `ParseError`), the TCK accepts these as
compile-time `SyntaxError` outcomes.

| Validator | Rejects |
|---|---|
| `VisitMapPair` digit-prefix check | Map keys whose first byte is `[0-9]` (e.g. `{1B2c3e67:1}`). |
| `VisitAtom` + `hasInvalidNumericChar` | Digit-prefixed ID tokens containing a letter outside the float-literal suffix set `eEfFdD` (e.g. `9223372h54775808`). |
| `VisitAtom` hex/oct overflow branch | `0x` (no digits), `0xABZ`, `0o9` (invalid octal digit), and any signed-decimal-out-of-range cousin. |
| `VisitProjectionItem` + `containsBareRelChainPattern` | A `relationshipsChainPattern` appearing as a RETURN / WITH projection value or anywhere inside a function argument (such as `size((a)-[:REL]->(b))`). |
| `VisitSetItem` + `containsBareRelChainPattern` | A `relationshipsChainPattern` appearing on the right-hand side of `SET propertyExpression = …` or `SET variable = …` / `SET variable += …`. |

`containsBareRelChainPattern` is a recursive walker that treats
`*ast.ExistsSubquery`, `*ast.CountSubquery`, and the pattern field of
`*ast.PatternComprehension` as opaque (those constructs legitimately
contain a pattern).

---

## Post-generation parser patches

Two classes of surgical edits are applied to the ANTLR-generated parser
after each regeneration. They are **not** captured by the grammar
(`.g4`) files — they live directly in `cypher/parser/gen/cypher_parser.go`
and must be re-applied after every `make generate-cypher-parser` run.

### A. Numeric-ID workarounds (task #375, refreshed in task #396)

The vendored lexer orders `ID: LetterOrDigit+` before `DIGIT`, so positive
integers like `3`, `42` lex as `ID` rather than `DIGIT`. The
`isNumericIDToken` helper and call-site edits in `Atom()`, `Literal()`,
`NumLit()`, and `RangeLit()` accept numeric `ID` tokens wherever the
grammar expects `DIGIT`. The helper functions live at the **end** of
`cypher_parser.go`, below all generated code, so the generator never
overwrites them.

When you regenerate, you must:
1. Restore the `isNumericIDToken` and `(p *CypherParser) rangeNumBound()`
   functions at the bottom of `cypher_parser.go`.
2. Re-apply the `if atomAlt == 11 && isNumericIDToken(...)` short-circuit
   inside `Atom()`.
3. Re-apply the `case CypherParserID:` arm and the conditional `Sync` skip
   inside `Literal()`.
4. Re-apply the numeric-ID branch inside `NumLit()`.
5. Re-apply the `rangeNumBound()` call sites inside `RangeLit()`.

### B. COUNT { … } subquery rule (task #396)

The grammar gained a `subqueryCount` rule alongside `subqueryExist` so
expressions of the form `COUNT { (n)-->() }` and `COUNT { MATCH … }` parse
as `*ast.CountSubquery`. The corresponding visitor method
`VisitSubqueryCount` lives in `cypher/parser/visitor.go` and is regenerated
into the generated visitor interface — no manual fix-up is required for
COUNT{} itself; only the **numeric-ID workarounds above** must be
re-applied after regeneration.

### C. Chained-WITH rewrite (task #376)

`MultiPartQ()` in the generated parser was patched to consume
`readingStatement*` segments interleaved with each `WITH` clause,
enabling `MATCH … WITH … MATCH … WITH … RETURN …` chains. The edit lives
inside `MultiPartQ()`; the helper logic is in-line so no end-of-file
helper needs to be restored. Re-apply this patch after each
regeneration.

### D. In-query CALL parentheses (task #43bdb24)

`QueryCallSt()` in the generated parser was patched so the argument
parentheses are optional, matching the behaviour of the standalone
`Call` rule. This pairs with the `normalizeCallNoParen` pre-processor
which inserts `()` when YIELD follows directly; the parser patch covers
the rarer case where YIELD is absent.
