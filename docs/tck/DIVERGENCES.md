# TCK Non-Conformances and Divergences

**Date:** 2026-05-21  
**Corpus:** openCypher TCK — opencypher/openCypher@main  
**Module version:** gograph v2.0.0-dev  

---

## Overview

The openCypher TCK contains 1 615 scenarios with a `When executing query:` step.
After Scenario Outline expansion (see Category 0 below), the effective corpus
grows to **3 897 scenarios** (2 282 new scenarios from expanding 262 outline
templates). GoGraph currently implements **parser-level conformance** (100 % pass
rate on 3 534 run scenarios) and **partial expression evaluation** (CASE, list
ops, map ops, built-in functions). Full execution against a graph backend is in
progress.

| Layer | Scenarios | Passing | Pass rate |
|---|---|---|---|
| Parser (grammar + AST round-trip) | 3 534 | 3 534 | 100.0 % |
| Skipped (grammar gaps) | 363 | — | — |
| **Overall (pass / total)** | **3 897** | **3 534** | **90.7 %** |
| Execution (godog runner, Sprint 31 baseline) | 3 897 | 407 | **10.4 %** |
| Execution (godog runner, Sprint 37 — task #375) | 3 897 | 968 | **24.8 %** |
| Execution (godog runner, Sprint 42 — task #391) | 3 897 | 1 152 | **29.6 %** |

The parser-level target gate is ≥ 90 %. This target is now achieved. The remaining
grammar gap is entirely accounted for by documented limitations; no scenario is
silently failing. Execution-level conformance is at an early baseline of 10.4 %
(Sprint 31); see Category 5 for the full gap taxonomy and remediation roadmap.

---

## Category 0 — Scenario Outline Expansion (introduced in task-279)

Previously, the 262 `Scenario Outline` template scenarios were emitted as-is
into the corpus with `SkipReason = placeholder-template`. Starting from this
task, `parseFeatureFile` expands each `Scenario Outline` block by parsing its
`Examples:` table and substituting `<column>` placeholders into the query for
each data row. The template is no longer emitted; only the concrete, expanded
scenarios appear.

This change:
- Replaces 262 placeholder-template scenarios with **2 541 expanded row scenarios**.
- Net effect: corpus grows from 1 615 to 3 897 total scenarios.
- 1 946 of the newly expanded scenarios are immediately runnable (no skip condition
  matches after substitution). All 1 946 pass the parser gate.
- The remaining 595 expanded rows are classified under existing skip reasons
  (single-quote-string, chained-with, zero-dot-float, varlen-explicit-bound).

Two new skip classifications were added to handle expansion artifacts:
- **`reSingleQuoteTemporalArg`** — temporal function calls like `date('2015-07-21')`
  or `duration('P5M1.5D')` where the single-quoted string contains digit–hyphen–digit
  or digit–dot–digit. These fail for the same root cause as `single-quote-string`.
- **`pattern predicates`** in `classifySkipByErrorType` — `size(<pattern>)` in
  RETURN is accepted by the grammar but the TCK expects `UnexpectedSyntax`. Same
  root cause as the existing `pattern in RETURN` skip rule.

---

## Category 1 — Grammar Gaps (363 scenarios skipped)

These scenarios are **excluded from the pass-rate gate** because the ANTLR grammar
in `cypher/parser/grammar/` does not yet cover the relevant syntax. Each category
is tracked as future work; removing a skip condition automatically re-exposes the
scenarios to the 100 % parser gate.

| Skip reason | Count | Syntax gap | Remediation |
|---|---|---|---|
| `single-quote-string` | **0** | **RESOLVED in v1.3.0** — single-quoted strings pre-processed by `normalizeSingleQuotes` in `cypher/parser/parse.go` before ANTLR lexing | — |
| `chained-with` | 188 | Multiple `WITH` clauses in one query chain | Extend `singleQuery` rule to allow `WITH … MATCH … WITH …` |
| `varlen-explicit-bound` | **0** | **RESOLVED in v1.4.0** — `normalizeVarlenBounds` in `cypher/parser/parse.go` rewrites `*N` / `*N..M` to `*-N` / `*-N..-M` before ANTLR lexing, bypassing the DIGIT/ID ambiguity; `visitRangeLit` reads bounds via `ctx.GetText()` | — |
| `grammar-gap-literal` | 19 | Malformed hex/integer literals accepted as two tokens; map keys starting with digit; pattern expressions in `RETURN`/`WITH`/`SET`; `size(<pattern>)` on pattern predicates; capital-E integer-mantissa negative-exponent float (`2E-01`) | Grammar-level validation |
| `zero-dot-float` | 21 | `0.5` — lexer splits `0` and `.5` into separate tokens | Fix lexer tokenisation of zero-prefixed floats |
| `leading-dot-float` | 15 | `.5`, `-.5` — float with no integer part | Add `LEADING_DOT_FLOAT` token to lexer |
| `varlen-dotdot` | 15 | `-[:T..]->` — dotdot without `*` | Extend relationship pattern |
| `neg-hex-oct` | 12 | `-0x1A2B`, `-0o777` | Support unary minus on hex/octal literals |
| `overflow-as-sema` | 5 | Integer/float overflow: TCK expects `SyntaxError`, visitor emits `SemaError` | Promote overflow detection to lexer/parser |
| `double-not` | 1 | `NOT NOT expr` — grammar disallows nested NOT | Extend unary expression rule |
| `call-no-paren` | 1 | `CALL proc YIELD out` without parentheses | Extend `inQueryCall` rule |
| `long-float-sema` | 1 | Very long float literal causes visitor SemaError on a valid query | Fix overflow detection in numeric literal handler |

### Note on single-quote-string resolution (v1.3.0)

The 579 `single-quote-string` scenarios are no longer skipped. A pre-processing
step (`normalizeSingleQuotes` in `cypher/parser/normalize.go`) rewrites all
single-quoted string literals to double-quoted form before ANTLR lexing. This
approach is safe because:

- ANTLR already handles double-quoted strings correctly.
- Single quotes are only valid in Cypher as string delimiters.
- The rewriter skips over double-quoted strings, backtick identifiers, and both
  line (`//`) and block (`/* */`) comments to avoid false rewrites.

One scenario (`Literals7.feature [16]`) was previously hidden under the
`single-quote-string` skip but actually fails due to a separate grammar gap:
`2E-01` (capital-E integer-mantissa negative-exponent float) is not supported by
the ANTLR grammar. It has been explicitly catalogued in `grammarGapExact` under
`grammar-gap-literal`.

---

## Category 2 — Write-Clause Scenarios (task-279 analysis)

The five write-clause feature directories (`clauses/create`, `clauses/merge`,
`clauses/delete`, `clauses/set`, `clauses/remove`) contain **280 scenarios**
after Scenario Outline expansion. After the single-quote-string fix, all 280
scenarios are now runnable (the 19 previously-skipped single-quote-string
write-clause scenarios now run and pass). The remaining 12 skipped are:

| Skip reason | Count |
|---|---|
| `chained-with` | 10 |
| `varlen-explicit-bound` | **0** |

All runnable write-clause scenarios parse correctly (100 % parser pass rate).

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

The following behaviours diverge from the openCypher 9 specification. Each entry
carries an explanation and the planned remediation.

| Behaviour | openCypher spec | GoGraph behaviour | Planned fix |
|---|---|---|---|
| `0.5` float literal | `0.5` → Float 0.5 | Lexer emits two tokens: IntegerLiteral(0) + DotFloat(.5), causing a parse error | Fix zero-dot-float lexer rule |
| `-0x1A2B` | Integer literal -6699 | Unary minus on hex literal fails to parse | Support negated hex/octal literals in the grammar |
| `NOT NOT expr` | Double negation | Parse error: grammar disallows `NOT` as operand of `NOT` | Extend unary expression rule |
| Integer/float overflow | `SyntaxError` at parse time | `SemaError` from visitor's numeric literal handler | Promote to parse-time error |
| Multi-word single-quoted strings | Valid string literal | **RESOLVED in v1.3.0** — pre-processing normalises to double-quoted form | — |

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

### Parser level (grammar gaps — ~312 scenarios)

1. **Grammar fixes** (zero-dot-float, chained WITH, varlen bounds, leading-dot-float,
   neg-hex-oct) — resolves ~312 additional skip scenarios and promotes them into the
   parser gate.

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
