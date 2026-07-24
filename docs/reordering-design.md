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
is provably suboptimal, out of scope); any pattern needing a **multi-edge
intermediate cardinality** (≥2 joins where a downstream intermediate is not a
single exact count-store entry — correlation-dependent, error compounds,
Ioannidis-Christodoulakis SIGMOD'91); **variable-length expand**;
**shortestPath/allShortestPaths**; any path touching a **dirty** `D`/`T` cell;
and (a separate correctness guard, not order) any permutation that would swap the
outer/inner role across an **OPTIONAL MATCH** boundary (changes the left-join
multiset).

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
