# TCK Parser-Only Report

**Date:** 2026-05-20  
**Corpus:** openCypher TCK — opencypher/openCypher@main  
**Grammar:** antlr/grammars-v4, commit 284602b (BSD-3)  
**Runner:** `gograph/cypher/tck`, test `TestTCKParserOnly`

---

## Summary

| Metric | Value |
|---|---|
| Total TCK scenarios with `When executing query:` | 3897 |
| Scenarios run against `parser.Parse` | **2983** |
| Scenarios skipped (grammar gaps, see below) | 914 |
| Pass rate on run scenarios | **100.0 %** |
| Overall pass rate (run / total) | **76.5 %** |

---

## Coverage by Feature Area

| Feature area | Total | Run | Pass | Skip | Pass% |
|---|---|---|---|---|---|
| clauses/call | 52 | 51 | 51 | 1 | 100.0% |
| clauses/create | 78 | 72 | 72 | 6 | 100.0% |
| clauses/delete | 41 | 37 | 37 | 4 | 100.0% |
| clauses/match | 381 | 329 | 329 | 52 | 100.0% |
| clauses/match-where | 34 | 27 | 27 | 7 | 100.0% |
| clauses/merge | 75 | 62 | 62 | 13 | 100.0% |
| clauses/remove | 33 | 33 | 33 | 0 | 100.0% |
| clauses/return | 63 | 61 | 61 | 2 | 100.0% |
| clauses/return-orderby | 35 | 28 | 28 | 7 | 100.0% |
| clauses/return-skip-limit | 31 | 30 | 30 | 1 | 100.0% |
| clauses/set | 53 | 45 | 45 | 8 | 100.0% |
| clauses/union | 12 | 12 | 12 | 0 | 100.0% |
| clauses/unwind | 14 | 14 | 14 | 0 | 100.0% |
| clauses/with | 29 | 24 | 24 | 5 | 100.0% |
| clauses/with-orderBy | 292 | 172 | 172 | 120 | 100.0% |
| clauses/with-skip-limit | 9 | 9 | 9 | 0 | 100.0% |
| clauses/with-where | 19 | 13 | 13 | 6 | 100.0% |
| expressions/aggregation | 35 | 26 | 26 | 9 | 100.0% |
| expressions/boolean | 150 | 143 | 143 | 7 | 100.0% |
| expressions/comparison | 72 | 51 | 51 | 21 | 100.0% |
| expressions/conditional | 13 | 1 | 1 | 12 | 100.0% |
| expressions/existentialSubqueries | 10 | 10 | 10 | 0 | 100.0% |
| expressions/graph | 61 | 58 | 58 | 3 | 100.0% |
| expressions/list | 185 | 155 | 155 | 30 | 100.0% |
| expressions/literals | 131 | 80 | 80 | 51 | 100.0% |
| expressions/map | 44 | 25 | 25 | 19 | 100.0% |
| expressions/mathematical | 6 | 5 | 5 | 1 | 100.0% |
| expressions/null | 44 | 40 | 40 | 4 | 100.0% |
| expressions/path | 7 | 4 | 4 | 3 | 100.0% |
| expressions/pattern | 50 | 44 | 44 | 6 | 100.0% |
| expressions/precedence | 121 | 119 | 119 | 2 | 100.0% |
| expressions/quantifier | 604 | 464 | 464 | 140 | 100.0% |
| expressions/string | 32 | 23 | 23 | 9 | 100.0% |
| expressions/temporal | 1004 | 647 | 647 | 357 | 100.0% |
| expressions/typeConversion | 47 | 39 | 39 | 8 | 100.0% |
| useCases/countingSubgraphMatches | 11 | 11 | 11 | 0 | 100.0% |
| useCases/triadicSelection | 19 | 19 | 19 | 0 | 100.0% |

---

## Skipped Scenarios — Grammar Gap Taxonomy

See the TCK skip-reason inventory in `TestTCKParserOnlySkipCoverage`.
See `docs/tck/DIVERGENCES.md` for full divergence documentation.

---

## Reproducing

```bash
go test -run TestTCKParserOnly ./cypher/tck/...
go test -v -run TestTCKParserOnly ./cypher/tck/...
go test -v -run TestTCKParserOnlySkipCoverage ./cypher/tck/...
go test -race -run TestTCKParserOnly ./cypher/tck/...
```
