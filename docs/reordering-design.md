# Count-Store-Gated Reordering — Design (P3, F1a/c + F2-core)

Status: design spike (#2088, sprint 307). No production code changed by this
document. Specialist inputs: graph-theory-expert (enumerator, admissibility
gate, cost model, reverse-expand hazard) and cypher-expert-consultant
(order-safety suppression). Date 2026-07-24.

## 0. Executive recommendation (decisive)

Under the absolute no-regression mandate, a DPccp search over ≤4 leaves
**collapses to two peepholes**: every ≥2-join candidate is vetoed (its
downstream intermediate cardinality is not a single exact count-store entry —
it is correlation-dependent, hence inexact — see §3). So the safe, shippable
scope is:

- **P-disjoint** — reorder disjoint comma-separated single-scan components so
  the smaller exact base cardinality builds first. Reverse-hazard-free.
  **Ship first (#2091).**
- **P-anchor** — for a single-edge pattern, choose the endpoint anchor that
  minimises examined edges. Depends on **expand reversal** (#2089) and is
  gated by the **reverse-expand efficiency** measurement (#2089a).

DPccp is retained only as the correctness argument and the growth path for a
future non-absolute `EstStats` mode; **no DP code ships now** — implement the
two peepholes directly.

## 1. The admissibility gate (code-ready)

A non-default plan `P` replaces the written-order plan `W` only when ALL hold:

1. **Scans exact.** Every leaf scan cardinality is `EstExact` (`N(label)` is
   always exact; all-nodes uses the exact total).
2. **Two-sided trustworthiness.** Every cost input on BOTH the candidate `P`
   path AND the baseline `W` path is `EstExact` **and not dirty**. (Both sides,
   because a fallback baseline of unknown magnitude could let the inequality in
   §1.5 pass falsely.) `E(relType)` is always exact; `D`/`T` are exact only when
   their cell is not dirty (a relabel can dirty them — see the count-store
   design §3.3.1).
3. **No fallback on the path.** Any `EstFallback` or unvalidated `EstHeuristic`
   anywhere on either path → veto.
4. **Order-safe.** `SuppressReorder(spine) == false` (§4).
5. **Cost margin.** `modeledCost(P) · margin ≤ modeledCost(W)` under one
   faithful cost model (§2), with `margin ≥ 1` a small constant-factor guard.

**Single-snapshot atomic reads.** A cost input's value and its provenance
(`EstExact`/dirty) must be sampled together from one count-store snapshot, so a
concurrent relabel cannot make a value read exact while its dirty flag is
missed.

Any veto → keep the written order. With no consumer today the whole thing is
inert; it lights up per-query exactly when the counts are trustworthy.

## 2. The cost model (single-edge and disjoint)

Left-deep index-expand cost is dominated by edges EXAMINED, a degree sum, not a
cardinality. For a leaf scan of label `L` and one expand over `(R, dir)`:

```
cost = c_s · N(L)  +  c_e^{dir} · D(L, R, dir)
```

- **P-anchor.** For `(a:A)-[:R]->(b:B)`: anchor-A cost ≈ `c_s·N(A) + c_e^OUT·D(A,R,OUT)`;
  anchor-B cost ≈ `c_s·N(B) + c_e^IN·D(B,R,IN)`. The produced-row term
  `T(A,R,B)` is identical for both and **cancels** in the comparison. Unlabelled
  endpoints stay exactly costable via the total node count and `E(R)`.
- **P-disjoint.** Order components by exact base cardinality (`N`/`E`), smaller
  build side first.

When every input is `EstExact ∧ ¬dirty`, `min` is provably ≤ the written-order
cost under the same model — that is the no-regression guarantee.

## 3. The veto set (restated for code)

Veto (keep written order) for: **cyclic patterns** (AGM/worst-case-optimal-join
boundary — Atserias-Grohe-Marx FOCS'08, Ngo et al PODS'12; binary-join left-deep
is provably suboptimal, out of scope) — **this veto STANDS, and sprint 315 did not
relax it; see the note below**; any pattern needing a **multi-edge
intermediate cardinality** (≥2 joins where a downstream intermediate is not a
single exact count-store entry — correlation-dependent, error compounds,
Ioannidis-Christodoulakis SIGMOD'91); **variable-length expand**;
**shortestPath/allShortestPaths**; any path touching a **dirty** `D`/`T` cell;
and (a separate correctness guard, not order) any permutation that would swap the
outer/inner role across an **OPTIONAL MATCH** boundary (changes the left-join
multiset).


> **Note on the cyclic veto after sprint 315 (#2157).** The veto above is a statement
> about **binary-join reordering**, and it remains correct and in force: a cyclic
> pattern's intermediate cardinality is not a single exact count-store entry, so a
> cost comparison between orderings would compound error. Nothing in sprint 315
> changed that.
>
> What sprint 315 added is a **different mechanism for the same shape**, which is why
> the two coexist without contradiction. `exec.ExpandIntersect`
> ([`design-wcoj-cyclic-patterns.md`](design-wcoj-cyclic-patterns.md)) does not
> *reorder* a cyclic pattern and does not *estimate* anything: it fuses the cycle's
> open middle hop and its closing seek into one operator driven by a sorted-set
> intersection, so the choice it makes is per-row and structural rather than
> cost-based. It consults **no count-store cell at all** — it orders its ranges by CSR
> run length, which is exact per-vertex and cannot be stale — so it introduces no new
> dependency on count-store freshness and nothing here needs a dirty-cell veto.
>
> The original veto was also the *signpost* that a different operator was required,
> and the SPIKE recorded why the obvious candidate is not it: every simple cycle
> admits exactly one intersection, at the vertex the `ExpandInto` seek already
> occupies, so a general worst-case-optimal join degenerates on a binary relation and
> is unconditionally out of scope.

## 4. Order-safety suppression (cypher-expert-consultant)

Reordering permutes row emission order of a bag; openCypher leaves that order
unspecified absent an order-establishing operator (openCypher 9, ORDER BY;
CIP2016-06-14). The TCK compares bags by default (`in any order:` = 1259
scenarios) and order only under `in order:` (86: 65 ORDER BY + 21 procedure
yield). **Decisive subtlety:** `in any order:` compares list cells EXACTLY, so
`collect()` over an order-unestablished stream is a **value** trap under the
common mode, not just under `in order:`.

Walk the spine from the reorder point to the root, nearest-ancestor first; first
decisive hit wins:

- **RESET (enabler):** a **total** `Sort`/`Top` above the reorder erases arrival
  order → safe above it. Totality is required (a non-total sort leaves tie-groups
  in arrival order); default `isTotalOrder` to false unless proven (unique-backed
  key, `elementId`, or full-tuple sort). `min`/`max` are the exception (range over
  global orderability, order-blind).
- **SUPPRESS (observers):** bare `LIMIT`/`SKIP` without a dominating total sort
  (changes WHICH rows survive — a multiset change); `collect()` (and
  `collect(DISTINCT)`); `head`/`last`/positional read of a collected list;
  non-commutative `reduce` (e.g. string concat); pattern comprehension
  (`RollUpApply`); `ProcedureCall` with a returned yield order; `UNWIND`
  (conservative); non-total `Sort`/`Top`.
- **NEUTRAL (pass through):** `Selection`, `Expand`/`OptionalExpand`/`VarLengthExpand`,
  `Apply`/`SemiApply`/`AntiSemiApply`, `Distinct`, order-blind aggregations
  (`count`/`sum(int)`/`avg(int)`/`min`/`max`/`stDev`/`percentile*`), `Union`/`UnionAll`,
  `SubqueryExists`/`SubqueryCount`, scalar `Projection`/`WITH`.

Reaching the root with no observer → result order unspecified → safe. The full
predicate (`SuppressReorder(spine)`) and operator table are in the
cypher-expert-consultant memory `spec-join-reorder-order-safety`; implement it as
specified. Per-`WITH`-segment analysis: a `WITH … ORDER BY <total>` is the enabler
for the next segment.

**Multiset/state hazards beyond order (hard flags, also veto):** bare LIMIT/SKIP
(which rows survive); exact **float** `sum`/`avg` as a compared output (IEEE-754
non-associativity); last-writer-wins `SET`/`Merge…SET` with a row-dependent RHS on
colliding targets; `id()`/`elementId()` generation order read into an
order-observed output.

## 5. The reverse-expand hazard — #2089a (empirical, gates #2090)

The P-anchor cost model assumes IN-expansion is `Θ(indegree)`. GoGraph stores
only OUT-adjacency; if `Expand` with `DirIn` is not `Θ(indegree)` (e.g. it scans
or is O(V+E)), then re-rooting a pattern to traverse a relationship in reverse is
not faithfully costed and could be slower than the written order. **#2089a**
measures the actual `Expand DirIn` cost empirically. Outcomes:

- **IN-expand is Θ(indegree):** anchor-swap is admissible in both directions.
- **IN-expand is not efficient:** restrict P-anchor to swaps that move the anchor
  toward the **OUT** traversal direction only (never introduce a reverse expand);
  or veto the swap. This is the mandate-preserving reduced scope.

### 5.1 Measurement verdict (#2089a) — OUT-only swap

> **⚠️ SUPERSEDED — 2026-07-29 (rmp #2150, measured under #2152).** The **policy**
> of this section no longer holds: the anchor swap is **symmetric**, and a
> reverse-introducing swap is now admitted. What is superseded is the *conclusion*,
> not the measurements — findings 1 and 2 below were correct when taken, and finding
> 1 (the reverse-CSR build cancels) still stands unchanged.
>
> Finding 2's per-in-edge table describes the **pre-#2142** code, where recovering a
> canonical edge id SCANNED the source's whole forward out-range. Since #2142 both
> lookups are binary searches, so the per-in-edge overhead is
> `Θ(log(out-degree))`, not `Θ(out-degree)` — and that is a change in KIND, which is
> what made finding 3 obsolete. An unmodelled `Θ(out-degree)` term is unbounded
> relative to the edge cost, so no constant margin can absorb it; a
> `Θ(log out-degree)` term is bounded by 64 levels and the margin can.
>
> Do not carry finding 2's figures forward as current. The recalibrated constants,
> the revised cost model and the no-regression argument are in **§5.2**;
> `cypher/anchor_swap_plan.go` is the implementation of record.

### 5.1.1 The original findings

Measured on Apple M4 (go1.26.5) via `cypher/exec/expand_reverse_cost_bench_test.go`
(`go test -run=^$ -bench='BenchmarkExpand|BenchmarkBuildReverse' -benchmem
./cypher/exec/`). Three findings decide the policy:

1. **The reverse-CSR BUILD cost is NOT the hazard — it cancels.** `csrPairFromGraph`
   (api.go) builds BOTH the forward and the reverse CSR unconditionally on every
   `Expand`, for a `DirOut` plan exactly as for a `DirIn` plan
   (`fwd = BuildFromAdjListLive(...); rev = fwd.BuildReverse()`). `BuildReverse` is
   `O(V+E)` (`BenchmarkBuildReverse_vs_E`: 58 µs / 0.50 ms / 4.6 ms at E =
   10k / 100k / 1M, linear). Because it is paid identically by anchor-A and
   anchor-B, it cancels in the P-anchor comparison — the design's omission of it
   from §2 is faithful.

2. **A bare IN traversal IS `Θ(indegree)`** — but the per-in-edge cost is *not*
   `Θ(1)`. For each in-edge `(src → cur)` the operator recovers a canonical edge
   id by scanning `src`'s WHOLE forward out-range (`Expand.lookupFwdEdgePos`,
   `O(out-degree(src))`), and, when a relationship-type filter is set — which a
   directed `(a:A)-[:R]->(b:B)` anchored at `b` always sets — a SECOND
   `O(out-degree(src))` scan (`Expand.reverseEdgePassesFilter`). Sweeping a single
   in-edge whose source has out-degree `K` (`BenchmarkExpandIn_PerEdge_vs_SourceOutdegree`):

   | K (source out-degree) | 16 | 256 | 4096 | 65536 |
   |---|---|---|---|---|
   | untyped IN, 1 in-edge | 181 ns | 293 ns | 1.81 µs | 26.2 µs |
   | typed IN, 1 in-edge   | 194 ns | 410 ns | 3.47 µs | 52.4 µs |

   Examining ONE in-edge into a hub source costs **~26–52 µs** versus **~19 ns**
   for one OUT edge (`BenchmarkExpandOut_PerEdge_SingleSource`: β_out ≈ 19 ns/edge;
   scan α ≈ 1.8 ns/node from `BenchmarkScan_PerNode`). The per-in-edge cost is
   `Θ(out-degree of the edge's source)` — a quantity the count-store's aggregate
   `D(label, relType, dir)` cannot express.

3. **Consequence:** a reverse-INTRODUCING swap (anchor-B with `DirIn`) can be
   constructibly slower than the written order even when the modeled edge counts
   favor it — a single in-edge into a mega-hub source dominates. `c_e^IN` cannot be
   a faithful constant, and no cost model over count-store aggregates can bound the
   overhead.

**Policy (matches §5's second bullet).** #2090 ships **OUT-ward swaps only**: the
anchor swap fires only when it moves the anchor so the resulting expand is `DirOut`
(it flips a written `DirIn` expand to `DirOut`), never when it would introduce a
`DirIn` expand. An OUT-ward swap REMOVES the reverse per-edge overhead; the
candidate (OUT) side is then faithfully modeled (`O(1)` per edge, no hidden scan)
while the baseline (written IN) side's true cost only exceeds its model (the
omitted reverse overhead is `≥ 0`). So `modeledCost(P) ≤ modeledCost(W) ⇒
trueCost(P) ≤ trueCost(W)` — no regression — for the cost model of §2 with `c_s :
c_e ≈ 1 : 8` (calibrated from α, β_out above; the ratio, not the absolute values,
is load-bearing) and a margin of 2 to absorb cross-machine ratio drift.
Reverse-introducing swaps and undirected (`DirBoth`) patterns are vetoed. A
forward-written `(a:A)-[:R]->(b:B)` whose cheaper anchor is `b` is therefore left
in written order (a missed optimization, never a regression); the beneficial,
shippable case is a written `DirIn` pattern (`(a:A)<-[:R]-(b:B)`) re-rooted onto
`b` as a `DirOut` expand.

### 5.2 Symmetric swap (#2150, measured under #2152)

The restriction above is **lifted**. The swap now fires in both directions;
undirected (`DirBoth`) patterns remain vetoed, because their mirror is also
`DirBoth` — the swap would move the anchor without changing the traversal — and
`D(label, relType, BOTH)` is not a modelled cell.

**What the missed optimisation was costing.** §5.1's own example — a
forward-written `(a:A)-[:R]->(b:B)` whose cheaper anchor is `b` — was not a small
loss. On a fixture where the hub's out-degree is 1601 and the whole `:Leaf` label
has one incoming edge, re-rooting is **12.4× faster; 331.8× at hub out-degree
40 000 and 2 303.7× at 200 000**. The swapped plan's cost is flat in the hub's
degree because it stops walking the hub at all, so the forfeited win grew without
bound.

**Recalibrated constants.** #2089a's `β_out ≈ 19 ns` was not carried forward: it
predates #2142 and measured the edge walk alone. The fixture is 4 000 `:A` and
4 000 `:B` nodes with `a_i → b_{(i+j) mod n}` for `j < d`, so out-degree(a) =
in-degree(b) = d and **both** directions scan n nodes and walk n·d edges — the only
difference is the reverse path.

| out-degree d | ns/edge OUT | ns/edge IN | ratio |
|---|---|---|---|
| 4 | 300.16 | 309.96 | 1.03 |
| 16 | 291.37 | 321.58 | 1.10 |
| 64 | 326.11 | 364.72 | 1.12 |
| 256 | 374.44 | 417.77 | 1.12 |
| 1 024 | 398.68 | 459.50 | 1.15 |

Fitting the **difference** of the two arms against `log₂(d)` — the difference,
because per-edge cost grows with d from cache pressure in *both* arms and that
growth would otherwise be charged to the probe — gives

```
in-edge penalty = 2.01 ns + 5.76 ns · log₂(d)
```

against a marginal out-edge of ≈400 ns. So the reverse overhead is 3% at degree 4
rising to only 15% at degree 1 024. Scaled against `c_e`, that is
`anchorSwapProbeCost = 12` per level of probe depth plus a base of 4, with the
constants rescaled `1 : 8 → 100 : 800` so the probe term stays a legible integer.
Rescaling both terms identically leaves every OUT-ward decision unchanged.

**The revised cost model.** The written OUT side keeps `c_s·N + c_e·D`. The IN-ward
candidate carries the probe term, charged **twice per in-edge** because a DirIn
traversal probes the source's forward range in `reverseEdgePassesFilter` and again
in `lookupFwdEdgePos[ByHandle]` — and a matched site always carries exactly one
relationship type, so the filter is always present and the factor 2 is never
avoidable:

```
cost_in = c_s·N(to) + ( c_e + probeBase + probeCost·⌈log₂(1 + D(from,R,OUT)) ⌉ ) · D(to,R,IN)
```

**Why no regression, given the model cannot be exact.** The depth term is an
*estimate*, not a bound, and deliberately so: `D` is an aggregate blind to degree
skew, **and** the mirror walks in-edges whose sources may carry any label (the
from-label Selection filters afterwards), so no from-label statistic bounds the
probed range. The guarantee instead rests on a **structural** bound — a probe depth
is at most 64 levels, so the per-in-edge penalty is at most `12·64 + 4 = 772 < 800`,
one modelled edge. A true in-edge therefore costs under *twice* a modelled
out-edge for any graph, and

```
true(candidate) ≤ 2·modelled(candidate, no probe term) ≤ modelled(written) = true(written)
```

because the gate admits only on a 2× modelled win and the written OUT side carries
no probe term, so its model is exact. That inequality is enforced by a declaration
in `anchor_swap_plan.go` that **fails to compile** if it stops holding, rather than
by a test that could be skipped; it was verified to bite by deliberately violating
it.

**Two extra gate conditions.** The IN-ward direction consumes `D(from,R,OUT)`
twice — as the written cost *and* as the probe depth — so the trustworthiness veto
covers it: without an exact, non-dirty from-side out-degree the candidate is not
costable at all. Note the dirty families are not symmetric (`countRelabel`): a
relabel **always** dirties `D(label,*,IN)`, because in-edges are not enumerable in
O(Δ) without a reverse index, but dirties `D(label,*,OUT)` only when the relabelled
node's out-degree exceeds the recount budget.

**One interaction to know about.** Admitting IN-ward swaps exposes
`(a:A)-[:K]->(m:B)` — the *common* written single-edge form — and re-rooting it
breaks the columnar chain, which the count-based model cannot see. Measured at
200 000 nodes on the columnar-eligible queries, swapping still wins by 1.31×–3.12×,
so the model's choice is right; but the model remains blind to execution mode, and
a future change to either side should know it.

Full measurements: [benchmarks/expand-into-symmetric-swap-2026-07-29.md](benchmarks/expand-into-symmetric-swap-2026-07-29.md).

## 6. Expand reversal invariants (#2089)

Re-rooting a single-edge pattern onto the other endpoint (traversing the
relationship in reverse) is result-SET-identical (same directed edges; only
emission order changes, handled by §4). Preserve: relationship-uniqueness /
cyphermorphism (order-independent set constraint), all variable bindings, and
data-direction fidelity — a `-->` rooted at its target expands IN over the same
edges; the re-root flips the START endpoint, never the semantic edge direction.

## 7. Ship order and sub-tasks

1. **#2091 P-disjoint** — disjoint-component ordering. Reverse-hazard-free; needs
   only the gate (§1) + order-safety (§4). Ship first.
2. **#2092 order-safety** — the `SuppressReorder` predicate (§4). Prerequisite for
   every peephole; land with or before #2091.
3. **#2089a reverse-expand measurement** — empirical `Expand DirIn` cost (§5).
4. **#2089 expand reversal** — the result-identical reversal operator (§6).
5. **#2090 P-anchor** — single-edge anchor swap (§2), gated by #2089a's outcome.
6. **#2093** differential + TCK tests; **#2094** benchmark; **#2095** EXPLAIN
   plan-diff + docs + KG; **#2119** example 24 exercise.

Every peephole is gated by §1 and §4, inert under a dirty/fallback path, and
differential-tested ON vs OFF for a byte-identical result multiset (and identical
order for the 86 in-order scenarios). TCK must stay 3897/3897.

## 8. No DP code now

The DPccp enumerator is NOT implemented — under §1 it produces only the two
peepholes. It is documented here as the correctness frame and the growth path:
when an `EstStats` mode (histograms, P4) later admits margin-gated multi-join
reordering, a bounded DPccp over ≤4 connected acyclic leaves becomes the vehicle,
reusing this gate with `EstStats` added to the trustworthy set. Until then,
peepholes only.
