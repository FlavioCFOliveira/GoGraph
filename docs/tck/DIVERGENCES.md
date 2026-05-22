# TCK Non-Conformances and Divergences

**Date:** 2026-05-22  
**Corpus:** openCypher TCK — opencypher/openCypher@main  
**Module version:** gograph v2.0.0-dev  

---

## Overview

The openCypher TCK contains 1 615 scenarios with a `When executing query:` step.
After Scenario Outline expansion (see Category 0 below), the effective corpus
grows to **3 897 scenarios** (2 282 new scenarios from expanding 262 outline
templates). After task #402 (Sprint 43) GoGraph achieves **full parser-level
conformance**: every scenario in the corpus is run through the parser and
every scenario passes. There are zero residual grammar gaps.

| Layer | Scenarios | Passing | Pass rate |
|---|---|---|---|
| Parser (grammar + AST round-trip) | **3 897** | **3 897** | **100.0 %** |
| Skipped (grammar gaps) | 0 | — | — |
| Execution (godog runner, Sprint 31 baseline) | 3 897 | 407 | **10.4 %** |
| Execution (godog runner, Sprint 37 — task #375) | 3 897 | 968 | **24.8 %** |
| Execution (godog runner, Sprint 42 — task #391) | 3 897 | 1 152 | **29.6 %** |

The parser-level target gate is ≥ 90 %. This target is now exceeded with
a clean 100 % pass rate across the full corpus. Category 1 (grammar gaps)
is closed for the parser tier; execution-level conformance continues to
grow under the remediation roadmap in Category 5.

---

## Category 0 — Scenario Outline Expansion (introduced in task-279)

Previously, the 262 `Scenario Outline` template scenarios were emitted as-is
into the corpus with `SkipReason = placeholder-template`. Starting from
task-279, `parseFeatureFile` expands each `Scenario Outline` block by parsing
its `Examples:` table and substituting `<column>` placeholders into the query
for each data row. The template is no longer emitted; only the concrete,
expanded scenarios appear.

This change:
- Replaces 262 placeholder-template scenarios with **2 541 expanded row scenarios**.
- Net effect: corpus grows from 1 615 to 3 897 total scenarios.
- After task #402 (Sprint 43), every expanded row is runnable; no skip
  classification remains active and the parser passes the full 3 897.

Two skip classifications were added during the expansion work; both have
since been retired:
- **`reSingleQuoteTemporalArg`** — temporal function calls like
  `date('2015-07-21')` or `duration('P5M1.5D')` where the single-quoted
  string contains digit–hyphen–digit or digit–dot–digit. Retired when
  `normalizeSingleQuotes` shipped in v1.3.0 — all such queries now run.
- **`pattern predicates`** in `classifySkipByErrorType` — `size(<pattern>)`
  in RETURN. Retired in task #402 by the visitor's
  `containsBareRelChainPattern` check, which raises a `SemaError` (a
  compile-time error in TCK terms) for the construct.

---

## Category 1 — Grammar Gaps (0 scenarios skipped)

This category previously catalogued every TCK scenario that could not pass
the parser gate because the ANTLR grammar in `cypher/parser/grammar/` was
either too restrictive (rejecting valid Cypher) or too permissive
(accepting input that the openCypher specification rejects). Task #402
(Sprint 43) closed the last open sub-classes; **no Category 1 entries
remain active**. The table below preserves the historical record so that
regressions can be traced back to the originating fix.

| Skip reason | Final count | Syntax gap | Resolution status |
|---|---:|---|---|
| `single-quote-string` | 0 | Multi-word single-quoted strings tokenised as char literal + identifier. | **RESOLVED in v1.3.0** — `normalizeSingleQuotes` in `cypher/parser/normalize.go` rewrites `'…'` → `"…"` before ANTLR lexing. |
| `varlen-explicit-bound` | 0 | `-[:T*N..M]->` numeric bounds emit ID tokens that the parser rejects. | **RESOLVED in v1.4.0** — `normalizeVarlenBounds` rewrites `*N..M` to `*-N..-M`; `visitRangeLit` absolute-values the result. |
| `chained-with` | 0 | Multiple `WITH` clauses in one query chain (`MATCH … WITH … MATCH … WITH …`). | **RESOLVED in task #376 (Sprint 38)** — `MultiPartQ()` in the generated parser was patched to consume `readingStatement*` segments interleaved with each WITH. |
| `varlen-dotdot` | 0 | `-[:T..]->` — dotdot range without leading `*`. | **RESOLVED in Sprint 38** — `normalizeVarlenDotDot` inserts the missing `*` before each `..` inside relationship brackets. |
| `zero-dot-float` | 0 | `0.5` — lexer splits `0` and `.5` into separate tokens. | **RESOLVED in Sprint 38** — `normalizeZeroDotFloat` rewrites `0.NNN` → `.NNN` before ANTLR lexing. |
| `leading-dot-float` | 0 | `.5`, `-.5` — float with no integer part, especially `.0NNN` forms. | **RESOLVED in Sprint 38** — `normalizeLeadingDotFloat` prepends `0` to leading-dot floats that begin with a zero so they tokenise as a single FLOAT. |
| `neg-hex-oct` | 0 | `-0x1A2B`, `-0o777` — unary minus on hex/oct literals. | **RESOLVED in task #43bdb24 (Sprint 38)** — `normalizeNegHexOct` rewrites the literal to its signed decimal form before lexing. |
| `overflow-as-sema` | 0 | Integer/float overflow: TCK expects `SyntaxError`, visitor emitted `SemaError`. | **RESOLVED in task #43bdb24 (Sprint 38)** — `IntegerOverflow` / `FloatingPointOverflow` are now listed in `parseTimeErrors`; the visitor's overflow error counts as a compile-time syntax error per the TCK. |
| `double-not` | 0 | `NOT NOT expr` — grammar's `notExpression` rule disallows nested NOT. | **RESOLVED in task #43bdb24 (Sprint 38)** — `normalizeDoubleNot` applies double-negation elimination (`NOT NOT x` → `x`, `NOT NOT NOT x` → `NOT x`) before lexing. |
| `call-no-paren` | 0 | `CALL proc YIELD out` — in-query CALL without argument parentheses. | **RESOLVED in task #43bdb24 (Sprint 38)** — `QueryCallSt()` in the generated parser was patched so the parentheses are optional, matching the standalone CALL rule. |
| `long-float-sema` | 0 | Very long float literal (>50 digits) caused visitor SemaError on a valid query. | **RESOLVED in task #43bdb24 (Sprint 38)** — `strconv.ParseFloat` handles very long but finite decimal literals correctly; the special-case rejection was removed. |
| `grammar-gap-literal` (capital-E negative-exponent float `2E-01`) | 0 | Lexer's `ExponentPart` requires `Digits` (`[1-9]`-led) for the exponent, so `2E-01` tokenises as `ID("2E")` + `DIGIT("-01")`. | **RESOLVED in task #402 (Sprint 43)** — `normalizeFloatExpZeroPad` strips redundant leading zeros from any signed exponent (`2E-01` → `2E-1`, `5e-001` → `5e-1`) before lexing, producing a single FLOAT token. |
| `grammar-gap-literal` (map key starting with a digit) | 0 | `{1B2c3e67:1}` — grammar accepts; spec rejects with `UnexpectedSyntax`. | **RESOLVED in task #402** — `VisitMapPair` rejects keys whose first byte is `0`-`9` with a `SemaError`, satisfying the TCK's parse-time error expectation. |
| `grammar-gap-literal` (malformed integer literal `9223372h54775808`) | 0 | Lexer accepted the digit-prefixed token as a bare ID; visitor silently treated it as a variable reference. | **RESOLVED in task #402** — `VisitAtom` calls `hasInvalidNumericChar` and rejects any digit-prefixed ID containing a letter outside `eEfFdD` with a `SemaError`. |
| `grammar-gap-literal` (incomplete / malformed hex literals `0x`, `0x1A2b3j4D5E6f7`, `0x1A2b3c4Z5E6f7`) | 0 | Visitor's hex-overflow branch already raised an error; the scenarios were skipped only because the entries in `grammarGapExact` had not been re-evaluated. | **RESOLVED in task #402** — the exact-pair entries were removed from `grammarGapExact`; the existing overflow branch in `VisitAtom` raises `SemaError("integer literal out of range")` which satisfies the TCK's `InvalidNumberLiteral` expectation. |
| `grammar-gap-literal` (invalid unicode escape `'\uH'`) | 0 | Lexer hides ERRCHAR fragments produced by the broken escape, leaving the parser unaware of the malformation. | **RESOLVED in task #402** — `validateUnicodeEscapes` runs before the pre-processor pipeline and rejects any `\u` not followed by exactly four hexadecimal digits with a `ParseError`. |
| `grammar-gap-literal` (invalid unicode operator character, em-dash in `42 — 41`) | 0 | Lexer's `ERRCHAR -> channel(HIDDEN)` hides the offending byte; the ANTLR error listener surfaces it as `unexpected "—"`. | **RESOLVED in task #402** — `InvalidUnicodeCharacter` is now listed in `parseTimeErrors`; the existing lexer surface error satisfies the TCK's compile-time expectation. |
| `grammar-gap-literal` (pattern expression in projection / SET / `size()` argument) | 0 | Grammar accepts `relationshipsChainPattern` as an `atom`; spec rejects it outside `MATCH` / `EXISTS{…}` / `COUNT{…}` / pattern comprehensions. | **RESOLVED in task #402** — the visitor calls `containsBareRelChainPattern` on every projection item and SET right-hand side; if the expression sub-tree contains a `*ast.PathPattern` outside an opaque sub-query / pattern-comprehension context, a `SemaError` is raised. |

### Note on single-quote-string resolution (v1.3.0)

The 579 `single-quote-string` scenarios are no longer skipped. A pre-processing
step (`normalizeSingleQuotes` in `cypher/parser/normalize.go`) rewrites all
single-quoted string literals to double-quoted form before ANTLR lexing. This
approach is safe because:

- ANTLR already handles double-quoted strings correctly.
- Single quotes are only valid in Cypher as string delimiters.
- The rewriter skips over double-quoted strings, backtick identifiers, and both
  line (`//`) and block (`/* */`) comments to avoid false rewrites.

### Note on the task #402 (Sprint 43) closure

Task #402 closed every residual `grammar-gap-literal` sub-class. The
changes are catalogued together so the historical record is easy to
trace:

- **`normalizeFloatExpZeroPad`** (pre-processor) — strips leading zeros
  from any signed floating-point exponent so `2E-01` → `2E-1`,
  `5e-001` → `5e-1`. Triggers only when the exponent has an explicit
  sign and at least one trailing non-zero digit; skips hex/octal,
  identifiers, strings, backticks, and comments.
- **`validateUnicodeEscapes`** (pre-lex validator) — scans the raw query
  for malformed `\u` escapes inside any string literal and returns a
  `ParseError` when fewer than four hexadecimal digits follow.
- **`VisitMapPair`** — rejects map keys whose first byte is `[0-9]`,
  reporting `SemaError("map key must start with a letter or underscore")`.
- **`VisitAtom` + `hasInvalidNumericChar`** — rejects any digit-prefixed
  ID containing a letter outside the float-literal suffix set
  `eEfFdD`, reporting `SemaError("invalid number literal")`.
- **`grammarGapExact`** — emptied; the four exact-pair entries it held
  (`Literals2 [11]`, `Literals3 [12]/[13]/[14]`) were redundant because
  the visitor's existing checks already produced the expected
  compile-time error.
- **`parseTimeErrors`** — `InvalidUnicodeCharacter` added so the existing
  lexer-level rejection of the em-dash counts as a compile-time syntax
  error per the TCK.
- **`containsBareRelChainPattern`** + visitor calls in
  `VisitProjectionItem` and `VisitSetItem` — recursively walks every
  expression sub-tree (excluding opaque subquery / pattern-comprehension
  contexts) and rejects any `*ast.PathPattern` found, with
  `SemaError("relationship-chain pattern is not allowed as a projection
  value")` / `("... as a SET right-hand side value")`.

All of the above are purely visitor- or pre-processor-level changes; the
ANTLR grammar in `cypher/parser/grammar/` was not regenerated and the
post-generation patches catalogued in `docs/tck/parser-report.md` are
unchanged.

---

## Category 2 — Write-Clause Scenarios (task-279 analysis)

The five write-clause feature directories (`clauses/create`, `clauses/merge`,
`clauses/delete`, `clauses/set`, `clauses/remove`) contain **280 scenarios**
after Scenario Outline expansion. After the cumulative grammar fixes through
task #402 (Sprint 43), all 280 write-clause scenarios are runnable and pass
the parser gate, including the pattern-in-RHS-of-SET case which is now
rejected at the visitor level.

All write-clause scenarios parse correctly (100 % parser pass rate).

---

## Category 3 — Execution Scenarios (baseline established Sprint 31)

All 3 897 TCK scenarios that contain `When executing query:` steps also specify
an expected result (`Then the result should be`, `Then a SyntaxError should be
raised`, etc.). The parser-level runner validates grammar correctness only.

The godog execution runner was added in Sprint 31 (`cypher/tck/runner_test.go`,
`cypher/tck/world_test.go`). It wires up `cypher/exec/` against the TCK harness
and reports execution conformance. The Sprint 31 baseline is:

- **407 / 3 897 scenarios passing (10.4 %)** at execution level (Sprint 31 baseline).
- **968 / 3 897 scenarios passing (24.8 %)** as of Sprint 37 (task #375).
- **1 152 / 3 897 scenarios passing (29.6 %)** as of Sprint 42 (task #391 —
  aggregation runner→engine→aggregator wiring). The net uplift over Sprint 37
  is ~184 scenarios, driven by (a) propagating group-by and aggregate-argument
  AST expressions into the EagerAggregation pre-projection so property
  accesses such as `n.name` and `n.num` resolve correctly; (b) the new
  `GlobalAggregateAdapter` that synthesises one row of neutral aggregate
  results when a group-by-less query runs over zero input rows; (c) TCK
  value formatting fixes in the godog comparator (single-quoted strings,
  `N.0`-form floats, node labels); (d) `stDev` / `stDevp` added to the
  aggregate-function detection set so the planner emits an EagerAggregation
  for them.

The execution engine (`cypher/exec/`) can evaluate `MATCH … WHERE … RETURN`
queries against an in-memory graph (`graph/lpg`). The following areas are not yet
fully wired up or implemented:

| Feature area | Reason for low pass rate |
|---|---|
| `clauses/match` | Multi-hop patterns, label predicates, cyclic patterns |
| `clauses/create` / `clauses/merge` | Merge operator not yet wired |
| `clauses/delete` / `clauses/set` / `clauses/remove` | Some write operator root-plan wiring missing |
| `expressions/temporal` | Temporal types (Date, DateTime, Duration) not yet implemented |
| `useCases/triadicSelection` | Requires path-pattern matching |
| Semantic validation | VariableTypeConflict, InvalidArgumentType, UndefinedVariable not yet checked |
| `UNWIND` | Wired (Sprint 37) but some list-expression evaluation edge cases remain |

See Category 5 below for the full execution-engine gap taxonomy with scenario
counts and remediation priority.

---

## Category 4 — Known Semantic Non-Conformances

The following behaviours previously diverged from the openCypher 9
specification. All of the entries originally tracked here have been
resolved; the table is preserved for historical context.

| Behaviour | openCypher spec | GoGraph behaviour | Resolution |
|---|---|---|---|
| `0.5` float literal | `0.5` → Float 0.5 | Lexer used to emit two tokens (`IntegerLiteral(0)` + `DotFloat(.5)`), causing a parse error. | **RESOLVED in Sprint 38** — `normalizeZeroDotFloat` pre-processes `0.NNN` to `.NNN`. |
| `-0x1A2B` | Integer literal -6699 | Unary minus on hex literal used to fail to parse. | **RESOLVED in Sprint 38** — `normalizeNegHexOct` rewrites the literal to signed decimal form. |
| `NOT NOT expr` | Double negation | Parse error: grammar's `notExpression` rule disallowed `NOT` as operand of `NOT`. | **RESOLVED in Sprint 38** — `normalizeDoubleNot` applies double-negation elimination before lexing. |
| Integer/float overflow | `SyntaxError` at parse time | Used to be `SemaError` from visitor's numeric literal handler. | **RESOLVED in Sprint 38** — `IntegerOverflow` / `FloatingPointOverflow` are now listed in `parseTimeErrors`, so a visitor overflow counts as a compile-time syntax error per the TCK. |
| `2E-01` capital-E negative-exponent float | Valid float literal 0.2 | Lexer split `2E` as ID and `-01` as DIGIT, causing a parse error. | **RESOLVED in task #402 (Sprint 43)** — `normalizeFloatExpZeroPad` strips redundant leading zeros from any signed exponent. |
| Multi-word single-quoted strings | Valid string literal | Parse error from the grammar tokeniser. | **RESOLVED in v1.3.0** — `normalizeSingleQuotes` rewrites `'…'` to `"…"`. |

---

## Category 5 — Execution Engine Gaps (Sprint 31 baseline)

The following gaps account for the bulk of the 3 490 failing execution scenarios
(89.6 % of the corpus). Counts are approximate; they are derived from feature-file
categorisation and godog progress output from Sprint 31.

| Gap | Affected scenarios (approx.) | Description |
|---|---|---|
| Property access on nodes/relationships | ~1 200 | `n.name`, `r.weight`, etc. evaluate to `null` in the execution engine instead of the node's property value. Affects nearly every `MATCH … RETURN n.prop` scenario. |
| MATCH with multiple patterns / OPTIONAL MATCH | ~800 | Multi-pattern MATCH and OPTIONAL MATCH are parsed correctly but the execution engine does not bind all pattern components to graph elements. |
| Aggregation functions | ~150 remaining (task #391 wired property-based group-by / aggregate-arg through the runner; the remaining failures depend on adjacent gaps — `0.5`/`1.0` float-literal tokenisation, OPTIONAL MATCH property load, `WITH … LIMIT` ordering, percentileCont/percentileDisc two-argument support) | `count(*)`, `count(n)`, `count(n.prop)`, `sum(n.prop)`, `avg(n.prop)`, `min(n.prop)`, `max(n.prop)`, `collect(n.prop)`, `stDev(n.prop)`, `stDevp(n.prop)` are now fully wired end-to-end through the godog runner; group-by keys built from property accesses (e.g. `MATCH (n) RETURN n.name, count(n.num)`) resolve correctly. |
| Path expressions and variable-length patterns | ~400 | `(a)-[*1..3]->(b)`, `shortestPath(…)`, and path variable assignment (`p = …`) require a path-expansion executor that is not yet implemented. |
| ORDER BY, SKIP, LIMIT execution | ~300 | The Sort, Skip, and Limit physical operators exist in `cypher/exec/` but are not wired to the godog execution harness result pipeline, so ordering and pagination tests fail. |

### Why the execution rate is 10.4 % and not 0 %

The 407 passing scenarios are ones where:

- The query has no `MATCH` clause and evaluates pure expressions (`RETURN 1 + 2`,
  `RETURN toUpper('hello')`).
- Error-expectation scenarios where GoGraph correctly raises a `SyntaxError` or
  `SemaError` for malformed queries.
- Simple `RETURN` with literal values and basic arithmetic that the expression
  evaluator already handles.

---

## Roadmap

The following tasks will close the remaining conformance gap, listed by priority:

### Parser level

The parser tier is complete: 3 897 / 3 897 scenarios pass after task #402
(Sprint 43). No further parser-level work is queued; see Category 1 for
the full closure record.

### Execution level (engine enhancements — ~3 490 scenarios)

2. **Property access on nodes and relationships** — wire property reads (`n.name`) in
   the MATCH result-binding stage; highest impact item (~1 200 scenarios).
3. **Multi-pattern MATCH and OPTIONAL MATCH** — complete pattern binding for queries
   with multiple comma-separated patterns and optional paths (~800 scenarios).
4. **Aggregation execution** — connect `EagerAggregation` operator to the TCK runner's
   result pipeline; implement `count`, `sum`, `avg`, `collect`, `min`, `max` (~600 scenarios).
5. **Path expressions and variable-length patterns** — implement path-expansion executor
   for `-[*N..M]->`, `shortestPath`, `allShortestPaths`, and path variable binding
   (~400 scenarios).
6. **ORDER BY / SKIP / LIMIT** — connect Sort, Skip, Limit physical operators to the
   godog result pipeline (~300 scenarios).
7. **Temporal types** — implement Date, DateTime, LocalDateTime, Duration values and
   their built-in functions (affects temporal expression scenarios).
8. **Subquery support** — EXISTS { } and COUNT { } subqueries. **PARTIALLY
   RESOLVED in task #396 (Sprint 42).** EXISTS{...} and COUNT{...} now compile
   and evaluate end-to-end:
   - `EXISTS{...}` works both as a top-level WHERE predicate (via the existing
     `SemiApply` / `AntiSemiApply` operators, now correctly wired to thread the
     `Argument` tag through the IR-to-exec build so the inner pipeline
     observes the outer row per iteration) and as a sub-expression inside
     arbitrary boolean predicates and RETURN items (via a new
     `expr.SubqueryEvaluator` interface that drives a compiled inner pipeline
     per outer row).
   - `COUNT{...}` is a new construct: the ANTLR grammar has been extended
     with a `subqueryCount` rule (mirroring `subqueryExist`), the visitor
     emits `*ast.CountSubquery`, and the expression evaluator drives the
     inner plan to completion, reporting the row count as `IntegerValue`.
   - New IR containers `ir.SubqueryExists` and `ir.SubqueryCount` were added
     for future plan-tree-pull rewrites; for now the expression evaluator is
     the canonical evaluation path.
   - Per openCypher semantics: EXISTS over empty match → `false`; COUNT over
     empty match → `0`. Outer-scope bindings are visible inside the subquery;
     inner-scope variables do not leak outwards.
   - The `cypher/tck/features/expressions/existentialSubqueries/` feature
     scenarios still fail in the godog runner because of an unrelated
     multi-pattern CREATE bug (the `CREATE (a:A)..., (a)-[:R]->...` syntax
     produces duplicate `a` nodes); this is tracked separately. Unit-level
     correctness of EXISTS/COUNT is asserted in
     `cypher/subquery_eval_test.go`.

### Milestones

| Milestone | Target pass rate | Key items |
|---|---|---|
| Sprint 32 | ~25 % execution | Property access + simple MATCH binding |
| Sprint 33 | ~45 % execution | OPTIONAL MATCH + multi-pattern |
| Sprint 34 | ~60 % execution | Aggregation functions |
| Sprint 35 | ~75 % execution | Path expressions + ORDER BY/SKIP/LIMIT |
| v2.0.0 | ≥ 90 % execution | Temporal types + subqueries + remaining grammar fixes |

### Resolved items

- **Grammar fixes (single-quoted strings)** — **RESOLVED in v1.3.0** via
  `normalizeSingleQuotes` pre-processor; 579 scenarios unblocked.
- **Grammar fixes (varlen-explicit-bound)** — **RESOLVED in v1.4.0** via
  `normalizeVarlenBounds`; 56 scenarios unblocked.
- **Grammar fixes (chained-with)** — **RESOLVED in task #376 (Sprint 38)**
  via `MultiPartQ()` parser patch; 188 scenarios unblocked.
- **Grammar fixes (zero-dot-float, leading-dot-float, varlen-dotdot)** —
  **RESOLVED in Sprint 38** via `normalizeZeroDotFloat`,
  `normalizeLeadingDotFloat`, and `normalizeVarlenDotDot` pre-processors;
  ~51 scenarios unblocked.
- **Grammar fixes (neg-hex-oct, double-not, call-no-paren,
  overflow-as-sema, long-float-sema)** — **RESOLVED in task #43bdb24
  (Sprint 38)** via the corresponding pre-processors and
  `parseTimeErrors` map updates; ~20 scenarios unblocked.
- **Grammar fixes — task #402 closure of every residual
  `grammar-gap-literal` sub-class (Sprint 43)** — closes the final 18
  parser-level scenarios. Closure mechanisms (see Category 1 for full
  detail):
  - `normalizeFloatExpZeroPad` — `2E-01` → `2E-1` (1 scenario,
    `expressions/literals/Literals7.feature [16]`).
  - `VisitMapPair` digit-prefix rejection — `{1B2c3e67:1}` (1 scenario,
    `expressions/literals/Literals8.feature [19]`).
  - `VisitAtom` + `hasInvalidNumericChar` — `9223372h54775808`
    (1 scenario, `expressions/literals/Literals2.feature [11]`).
  - `grammarGapExact` emptied — `0x`, `0x1A2b3j4D5E6f7`,
    `0x1A2b3c4Z5E6f7` (3 scenarios, `expressions/literals/Literals3.feature
    [12]/[13]/[14]`) were already rejected by the visitor's existing
    overflow branch; removing the exact-pair entries promotes them to
    runnable.
  - `validateUnicodeEscapes` pre-lex pass — `'\uH'` (1 scenario,
    `expressions/literals/Literals6.feature [13]`).
  - `InvalidUnicodeCharacter` added to `parseTimeErrors` — em-dash
    arithmetic `42 — 41` (1 scenario,
    `expressions/mathematical/Mathematical3.feature [1]`).
  - `containsBareRelChainPattern` rejection in `VisitProjectionItem` and
    `VisitSetItem` — pattern-in-projection / SET-RHS / `size()` argument
    (10 scenarios across `expressions/pattern/Pattern1.feature [22]/[23]/
    [24]` and `expressions/list/List6.feature [6]` rows).
- **Execution-level TCK runner (godog)** — **IMPLEMENTED in Sprint 31**;
  baseline execution pass rate 10.4 % (407/3897 scenarios) established.
- **Aggregation wiring through the godog runner** — **RESOLVED in Sprint 42
  (task #391)**. The `EagerAggregation` operator now receives parsed AST
  expressions for both grouping keys (`ir.EagerAggregation.GroupByExprs`)
  and aggregate-function arguments (`ir.AggregateExpr.ArgumentExpr`); the
  pre-projection evaluates them via `expr.Eval` against the
  pre-aggregation row context, so property accesses such as `n.name` and
  `n.num` produce the actual property values rather than the raw node id.
  A new `cypher/exec/global_aggregate_adapter.go` operator synthesises the
  single neutral-result row required by openCypher when a group-by-less
  aggregation runs over zero input rows (`count(*) → 0`, others → NULL).
  The runner's value-to-string formatter in `cypher/tck/compare_test.go`
  now quotes strings, preserves the `.0` suffix on integer-valued floats,
  and renders nodes as `(:Label)`.
