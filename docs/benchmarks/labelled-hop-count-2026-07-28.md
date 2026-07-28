# Counting a labelled single hop from the adjacency (rmp #2235)

**Date:** 2026-07-28 · **Sprint:** 327 · **Host:** Apple M4 (10 cores, 32 GB).

## 1. What was slow

`COUNT { (a)-[:K]->(:P) } > 0` and the pattern predicate `(a)-[:K]->(:P)` each drove a full
inner plan per outer row: plan `Init`, `Argument` seeding, row materialisation and neighbour
resolution, all to answer a question the anchor's adjacency already contains.

## 2. Why the degree rewrite could not take it

This is **not** a widening of #2232, and building it as one would have been wrong. #2232
implements Neo4j's degree rewrite, whose recogniser (`QuerySolvableByGetDegree`) requires
`Selections.empty` — and a label on a pattern node *is* a Selection (`HasLabels`) in Neo4j's
query graph. A labelled far node is ineligible for a degree rewrite in Neo4j too, for a
structural reason: a degree counts every out-edge and cannot ask anything about where an edge
lands.

So this is a separate optimisation in `cypher/labelled_hop_count.go`, with its own recogniser
and its own runtime counter. #2232's recogniser is left exactly as narrow as Neo4j's — the
round-3 lesson being that widening a recogniser steals shapes from other, cardinality-reducing
rewrites.

## 3. What it does

One walk of the anchor's adjacency, counting edges whose relationship type matches and whose
far endpoint carries every required label. Θ(d) where a degree is Θ(1), but it replaces the
whole inner-plan drive, and the constant is what dominates at property-graph degrees.

- **Membership** is tested with `label.Index.Has` through the graph's own node index — the same
  index the label scan reads — so the count and a plan that scanned the label cannot disagree
  about which nodes carry it.
- **Liveness** is not tested here at all. It belongs to
  `lpg.Graph.OutDegreeMatchingBoundedByID`, which applies the same tombstone gate as every other
  degree walker, so this path cannot drift from them.
- **Short-circuiting** is real: a comparison against a small literal stops the walk at the
  cap. `TestLabelledHopCount_ShortCircuits` measures the number of label probes on a degree-5000
  anchor — 3 probes under a cap of 3, and 5000 uncapped, because a walk that always stopped
  early would pass the capped assertion vacuously.

## 4. Measurement

`bench/r4audit` `TestPerOuterRowCost`, µs per outer row at N=8000. The baseline arm is the same
binary with the labelled-hop hooks removed, so only this change differs.

| Shape | Before | After | vs bare scan | Gain |
|---|---|---|---|---|
| `WHERE COUNT { (a)-[:K]->(:P) } > 0` | 2.308 | **0.518** | 85.5× → **18.5×** | **4.5×** |
| `WHERE (a)-[:K]->(:P)` | 0.736 | **0.502** | 27.3× → **17.9×** | **1.47×** |
| `EXISTS { MATCH … RETURN b }` (control) | 0.489 | 0.507 | — | unchanged |
| `baseline-scan` | 0.027 | 0.028 | 1.0× | unchanged |

The **before** figures reproduce the round-4 audit's 82.6× and 26× almost exactly (85.5× and
27.3×), which is what makes the attribution sound rather than assumed. The `exists-subquery`
control uses the full subquery form and is ineligible for this path; it does not move, so the
gain is the optimisation and not the harness or the host.

`TestPerOuterRowCost_DegreeEligible` records the same shape as `INELIGIBLE far label` —
named for its ineligibility to the *degree* rewrite — moving from 2.335 to 0.521 µs per row,
86.9× to 19.0× of the bare-scan baseline.

## 5. Correctness evidence

`TestLabelledHopCount_*`, all against a semantically equivalent unrewritable control **and** a
hand-computed absolute value:

- Zero matching neighbours; some matching and some not; an untyped hop; a label every
  neighbour carries; a multi-label far node as a conjunction; a label no node carries; a
  relationship type no edge carries.
- A far-node label added later by `SET` — the shape caches resolved label *ids*, and an id is
  stable, but membership is not.
- A tombstoned far node.
- A parallel-edge multigraph case, which is what surfaced **rmp #2241**: the typed degree walk
  under-counted parallel edges, so this case could not pass until that was fixed. See
  [`parallel-edge-typed-degree-2026-07-28.md`](parallel-edge-typed-degree-2026-07-28.md).
- Eleven ineligible shapes that must **not** take the path: a far-node property, a label on the
  anchor, an incoming or undirected hop, two relationship types, a named relationship variable,
  a variable-length range, two hops, both endpoints bound, an unlabelled far node, and an
  inline WHERE on the EXISTS spelling.

`labelledHopDifferential` additionally asserts that `degreeRewriteCount` does **not** move,
which discharges the requirement that #2232's recogniser is unchanged by construction rather
than by inspection: had this been built by widening it, every case would fail.

### Two oracles had to be discarded as invalid

The obvious control, `COUNT { (a)-[:K]->(b) WHERE b:Q }`, is **not** a valid oracle: the parser
drops the inline WHERE of a COUNT pattern subquery (rmp #2242), so it silently answers the
unfiltered count. And `EXISTS { (a)-[:K]->(b) WHERE b:Q }` was invalid for the same family of
reason until #2241's commit fixed it. The controls used here are the full subquery form,
`COUNT { MATCH … WHERE … RETURN b }`, which honours its predicate.

Both were caught by the absolute oracle, not by the differential — two forms agreeing proves
nothing when the bug is in the form you trusted.

## 6. Gates

- `make ci` green: tidy, fmt, vet, build, `go test -race` short layer, `golangci-lint`,
  cover-gate.
- openCypher TCK **3897/3897 scenarios, 0 failed, 0 undefined**.
