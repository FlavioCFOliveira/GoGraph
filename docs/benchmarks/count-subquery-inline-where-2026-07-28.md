# `COUNT { pattern WHERE predicate }` dropped its predicate (rmp #2242)

**Date:** 2026-07-28 · **Sprint:** 327.

This is a correctness record, not a performance one: there is no before/after timing because
the defect was a wrong answer, and the fix removes no work.

## 1. The defect

On a graph where `n3` has two `:K` out-edges, of which exactly one lands on a `:Q` node:

```
MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:K]->(b) }                            -> 2   correct
MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:K]->(b) WHERE b:Q }                  -> 2   WRONG
MATCH (a:P {id: 3}) RETURN COUNT { MATCH (a)-[:K]->(b) WHERE b:Q RETURN b }   -> 1   correct
MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:K]->(b) WHERE b.id = 999 }           -> 2   WRONG
```

The last line is the unambiguous one: a predicate that can never hold, and the unfiltered count
returned anyway.

## 2. Root cause

`ast.CountSubquery` carried **no `Where` field at all**, where its `ast.ExistsSubquery` sibling
had one. `VisitSubqueryCount` (`cypher/parser/visitor.go`) called `v.visitPatternWhere(pw)` —
which *does* parse the clause — and then built the node from `pat.Pattern` alone, discarding
`pat.Where`. The EXISTS branch ninety lines above sets `Where: pat.Where`, and its comment
states plainly that omitting it would silently drop the predicate.

The documentation asserted the opposite: `docs/cypher.md` said "an inner `WHERE` is applied in
every form". It was documenting intent rather than implementation.

## 3. The fix

Mirrors the EXISTS path exactly:

1. `ast.CountSubquery` gains `Where *Where`, and `String()` renders it, so the AST cannot print
   as a query with different meaning from the one parsed.
2. `VisitSubqueryCount` sets `Where: pat.Where`.
3. `countToSingleQuery` threads it into the inner `ast.Match`, which is where the ordinary MATCH
   pipeline turns it into a Selection.
4. **Both recognisers now receive it and therefore refuse the pattern.** `EvalCount` and
   `EvalCountBounded` previously passed `nil` for `where` — not by oversight, but because there
   was no field to pass — so a COUNT carrying an inline WHERE was eligible for the degree
   rewrite (#2232) and the labelled-hop count (#2235) and was answered *without* its predicate.
   A WHERE is a Selection neither can evaluate, so both must decline.

Point 4 is the part a reader should not skip: fixing the parser without it would have moved the
wrong answer from "the predicate is dropped" to "the predicate is dropped only when the pattern
happens to be degree-shaped", which is harder to notice.

## 4. Evidence

- `TestCountSubquery_HonoursInlineWhere` — nine bodies (no predicate, label, a predicate that
  can never hold, property equality, comparison, conjunction, negation, a predicate on an
  already-labelled far node, untyped hop) each checked against the full-subquery form **and** a
  hand-computed absolute value. The fixture's own facts are pinned first with an enumerating
  pattern comprehension, so a failure is the classifier's and not a wrong assumption.
- `TestCountSubquery_InlineWhereIsRefusedByBothRecognisers` — asserts on both runtime counters
  that neither rewrite fires for five WHERE-carrying shapes, including the bounded `> 0` form.
- `TestCountSubquery_StringRoundTripsWhere` — round-trips through parse and render, so it also
  proves the parser populated the field: a rendering can only keep what the parse preserved.
- `docs/cypher.md` gains a worked bare-pattern-with-`WHERE` example for `COUNT`. The
  documentation gate **executes** the examples in that file since #2227, so the doc is now a
  regression guard rather than a claim.

All three tests were verified to fail with the parser fix reverted — and the first attempt at
that verification reverted the *EXISTS* branch by mistake, because both branches contain the
same two lines; the tests stayed green and briefly suggested the fix was not load-bearing.

## 5. Gates

- `make ci` green: tidy, fmt, vet, build, `go test -race` short layer, `golangci-lint`,
  cover-gate (aggregate 86.9 %).
- openCypher TCK **3897/3897 scenarios, 0 failed, 0 undefined**. No TCK scenario covers this:
  `COUNT { … }` is a Neo4j 5 / GQL construct outside openCypher 9, which is why the suite was
  green with the defect present.

## 6. Provenance

Found while implementing #2235, which chose `COUNT { (a)-[:K]->(b) WHERE b:Q }` as its
differential control and had to abandon it. See
[`labelled-hop-count-2026-07-28.md`](labelled-hop-count-2026-07-28.md) and
[`parallel-edge-typed-degree-2026-07-28.md`](parallel-edge-typed-degree-2026-07-28.md) — the
EXISTS half of this same family was fixed there.
