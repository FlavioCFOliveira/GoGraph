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
| Scenarios run against `parser.Parse` | **3878** |
| Scenarios skipped (grammar gaps, see below) | 19 |
| Pass rate on run scenarios | **100.0 %** |
| Overall pass rate (run / total) | **99.5 %** |

---

## Coverage by Feature Area

| Feature area | Total | Run | Pass | Skip | Pass% |
|---|---|---|---|---|---|
| clauses/call | 52 | 52 | 52 | 0 | 100.0% |
| clauses/create | 78 | 78 | 78 | 0 | 100.0% |
| clauses/delete | 41 | 41 | 41 | 0 | 100.0% |
| clauses/match | 381 | 381 | 381 | 0 | 100.0% |
| clauses/match-where | 34 | 34 | 34 | 0 | 100.0% |
| clauses/merge | 75 | 75 | 75 | 0 | 100.0% |
| clauses/remove | 33 | 33 | 33 | 0 | 100.0% |
| clauses/return | 63 | 63 | 63 | 0 | 100.0% |
| clauses/return-orderby | 35 | 35 | 35 | 0 | 100.0% |
| clauses/return-skip-limit | 31 | 31 | 31 | 0 | 100.0% |
| clauses/set | 53 | 53 | 53 | 0 | 100.0% |
| clauses/union | 12 | 12 | 12 | 0 | 100.0% |
| clauses/unwind | 14 | 14 | 14 | 0 | 100.0% |
| clauses/with | 29 | 29 | 29 | 0 | 100.0% |
| clauses/with-orderBy | 292 | 292 | 292 | 0 | 100.0% |
| clauses/with-skip-limit | 9 | 9 | 9 | 0 | 100.0% |
| clauses/with-where | 19 | 19 | 19 | 0 | 100.0% |
| expressions/aggregation | 35 | 35 | 35 | 0 | 100.0% |
| expressions/boolean | 150 | 150 | 150 | 0 | 100.0% |
| expressions/comparison | 72 | 72 | 72 | 0 | 100.0% |
| expressions/conditional | 13 | 13 | 13 | 0 | 100.0% |
| expressions/existentialSubqueries | 10 | 10 | 10 | 0 | 100.0% |
| expressions/graph | 61 | 61 | 61 | 0 | 100.0% |
| expressions/list | 185 | 177 | 177 | 8 | 100.0% |
| expressions/literals | 131 | 124 | 124 | 7 | 100.0% |
| expressions/map | 44 | 44 | 44 | 0 | 100.0% |
| expressions/mathematical | 6 | 5 | 5 | 1 | 100.0% |
| expressions/null | 44 | 44 | 44 | 0 | 100.0% |
| expressions/path | 7 | 7 | 7 | 0 | 100.0% |
| expressions/pattern | 50 | 47 | 47 | 3 | 100.0% |
| expressions/precedence | 121 | 121 | 121 | 0 | 100.0% |
| expressions/quantifier | 604 | 604 | 604 | 0 | 100.0% |
| expressions/string | 32 | 32 | 32 | 0 | 100.0% |
| expressions/temporal | 1004 | 1004 | 1004 | 0 | 100.0% |
| expressions/typeConversion | 47 | 47 | 47 | 0 | 100.0% |
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
