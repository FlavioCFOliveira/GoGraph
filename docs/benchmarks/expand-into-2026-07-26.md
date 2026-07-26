# Expand-into — measured

**Task #2206** · sprint 324 · 2026-07-26 · Apple M4 (10 cores, `darwin/arm64`) ·
`go test -run=^$ -bench='BenchmarkExpandInto' -benchmem -count=5 -benchtime=5x ./cypher/`

2 000 nodes, out-degree 16 in each direction, all one label and one relationship type.
Benchmarks in `cypher/expand_into_bench_test.go`; correctness in
`cypher/expand_into_test.go`.

---

## 1. What was missing

A pattern that closes a cycle — `MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a)` — or otherwise
re-uses an already-bound endpoint has a final hop whose destination is **fixed**. The IR
translator already knows this: `matchExpandStepBoundWithFrom` sets `destRebinding`, expands
into a synthetic `__anon_N_to_<var>`, and puts an equality `Selection` above. `Engine.Explain`
shows it plainly:

```
Projection └─ Selection └─ Expand (c)-[:K]->(__anon_3_to_a) └─ Selection └─ Expand (b)-[:K]->(c) …
```

But the operator did not use that knowledge. It emitted **one row per neighbour** of `c`,
building and boxing a `(srcID, edgeID, dstID)` triplet for each, and the `Selection` above
discarded all but the one that landed on `a`. On a triangle that is the whole adjacency
materialised and thrown away, per input row.

The round-3 audit measured triangle counting at **107×** Memgraph's and named the missing
expand-into as the cause; round 2 had also flagged it, and round 3 re-prioritised it ahead
of the WCOJ work because a measured 107× deficit is not the place to start from.

## 2. What changed, and what deliberately did not

The filter is a **destination column on the existing `Expand`**, not a new operator. That is
the whole design decision. `Expand` is 907 lines carrying relationship isomorphism
(cyphermorphism), the multiplicity queue, forward/reverse/both traversal, the edge-type
filter, handle lookups and tombstone skipping. A parallel `ExpandInto` operator would have
had to reproduce every one of those guarantees; adding one integer comparison at the
**shared emit gate** (`tryFwdEdge` / `tryRevEdge`, which the columnar path also routes
through) inherits them all unchanged.

The IR gained an explicit `Expand.IntoVar` field rather than having the builder parse the
`__anon_N_to_<var>` naming convention. The convention stays — `lastSyntheticToFor` threads
the synthetic as the next hop's `FromVar` — but a planner decision should not hinge on a
string suffix.

The equality `Selection` above is **deliberately left in place**. The filter admits any
edge whose destination cell it cannot compare unboxed (a NULL, or a boxed entity a
projection put there), so the operator's output stays a **superset** of the correct result
and the `Selection` remains the source of truth. That is the same
seek-superset-plus-residual-refilter discipline the range seek uses, and it is what makes
the filter safe to apply *before* the `Selection` rather than instead of it.

## 3. Measured

| Query | before | after | speed-up |
|---|---|---|---|
| `(a)-[:K]->(b)-[:K]->(c)-[:K]->(a)` | 4.410 s | **274.8 ms** | **16.1×** |
| `(a)-[:K]->(b)-[:K]->(a)` | 225.7 ms | **16.78 ms** | **13.5×** |

| | before | after | |
|---|---|---|---|
| triangle allocs/op | 57.23 M | **2.045 M** | **−96.4 %** |
| triangle B/op | 1199.8 MiB | **28.36 MiB** | **−97.6 %** |
| 2-cycle allocs/op | 3.581 M | **132.4 k** | **−96.3 %** |
| geomean allocs | 14.32 M | 1.785 M | **−96.4 %** |

The open controls — the same shapes with a *fresh* final destination, where no
expand-into applies — are 3.846 s (triangle) and 189.6 ms (two-hop). So the closing
variants are now **faster than their open counterparts**, which is the right relationship:
a bound destination is strictly less work than a free one. Before this change the closing
triangle was *slower* than the open one (4.410 s against 3.846 s), because it did all the
open query's work and then threw most of it away.

## 4. Correctness

The tests do not compare the engine against itself. They compare it against an **oracle
computed in Go from the fixture**, which cannot share a bug with the operator:

- 2-cycle and triangle counts over a bidirectional ring, both against oracles built from a
  plain adjacency map. Each test asserts its oracle is non-zero, so a fixture that happened
  to contain no cycles fails loudly instead of passing vacuously — which it did on the first
  attempt, when a forward-only ring turned out to have neither.
- **Parallel edges preserve cardinality.** Two parallel `a->b` edges plus one `b->a` yields
  four rows, not two: the pattern matches in both orientations, and each parallel edge is a
  distinct relationship. A filter that answered "does an edge exist between this pair"
  instead of enumerating the edges between it would report two. This is the single most
  important case, because it is the one an "existence probe" implementation gets wrong.
- Self-loops, and the inbound and undirected forms of a closing hop, where cyphermorphism
  decides whether one edge may fill two pattern slots.
- Projected values, not just counts, so a filter applied at the wrong point cannot keep the
  right number of rows while pairing the wrong endpoints.

Test power verified by mutation: inverting the destination comparison fails three of the
four tests, including the oracle comparison at 0 pairs against 320 expected.

TCK 3897/3897 — the closing-hop shapes are TCK-covered, so the equality-Selection
semantics are exercised by the conformance suite as well.

## 5. What this does not do

The filter walks the same adjacency slice `Expand` already walks, so it is **O(degree)**,
not O(1) or O(log degree): the win is eliminating per-neighbour row construction, a large
constant factor, not an asymptotic one. Making the probe itself sublinear needs the
adjacency sorted by destination, which is round 2's degree-adaptive-sorting finding
(crossover measured at degree ≈ 64). The two compose — sorted adjacency would turn this
filter into a binary search — and should be planned together.
