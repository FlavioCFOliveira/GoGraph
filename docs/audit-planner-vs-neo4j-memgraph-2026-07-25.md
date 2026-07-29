# Planner evaluation vs Neo4j and Memgraph — 2026-07-25

Exhaustive evaluation of GoGraph's Cypher planner against the planners of
Neo4j and Memgraph, to determine what GoGraph can do **faster and more
efficiently** than either incumbent.

Baseline: **v0.10.0** (commit `0007214`), which shipped the complete planner
roadmap P1–P6 (sprints 305–310): min-cardinality multi-label anchor scan, the
exact relationship count-store, count-store-gated reordering, planner
statistics, columnar-execution deepening, and automatic intra-query
parallelism broadening.

## Method

Three evidence sources, in the order the project's *never guess* rule requires:

1. **Incumbent behaviour** — the web-verified reference facts for both
   planners (cost model, IDP bounds, statistics, runtimes, plan cache,
   Memgraph 3.8 parallelism), and the design-validation analysis of what is
   admissible for an LPG planner under an absolute no-regression mandate.
2. **GoGraph behaviour** — read from the source, never from memory: the
   physical build peepholes in `cypher/api.go`, the IR translator in
   `cypher/ir/match.go`, the operator set in `cypher/exec/`, the access paths
   in `graph/index/`, and the adjacency representation in
   `graph/adjlist/adjlist.go` and `graph/csr/csr.go`.
3. **Measurement** — every performance claim below is measured on this
   machine (Apple M4, `darwin/arm64`, Go benchmarks, `-count=5`/`-count=6`,
   `-benchmem`), with each pair of variants proven to return the identical
   result before its timings were compared, and each plan shape confirmed by
   `Engine.Explain` rather than inferred from timing.

Section 2 is the answer to the question asked. Sections 1 and 3 bound it
honestly on either side.

---

## 1. What GoGraph already does better

These are settled, and each is verified against the incumbents' documented
behaviour rather than assumed.

| Capability | GoGraph | Neo4j | Memgraph |
|---|---|---|---|
| Columnar / vectorized execution | **Yes** — unboxed chunk pipeline through scan → expand → filter → aggregation, with late-materialized hash join | No — the `pipelined` runtime's morsel is a *batch of rows*, not a column vector | No — row-at-a-time Volcano |
| Intra-query parallelism tier | **Automatic, OSS, no opt-in**, governed near `GOMAXPROCS` across concurrent queries | Enterprise, opt-in, **read-only** (5.13+) | Enterprise, opt-in via `USING PARALLEL EXECUTION` (3.8+) |
| Relationship count statistic | **Exact `(labelA, relType, labelB)` triple** | Single end-label only — deliberately not both | Cardinality × preset coefficient |
| Plan latency and stability | O(pattern) peepholes; no search, no stats-drift replan | IDP search bounded by table/duration thresholds; can fail with *unable to find a plan*; replans on stats divergence | Generates up to `--query-max-plans=1000` candidates, then costs them |
| Read-path synchronisation | Immutable snapshot; **zero synchronisation in the traversal hot loop** | MVCC record/page access | MVCC (delta-based) |
| Read–write isolation barrier | Snapshot isolation; `Eager` inserted in **one** narrow case (MERGE over DELETE) | Planner-inserted `Eager` for read-write dependencies — materialises all input, a documented OOM hazard | — |

The last row deserves emphasis because it is an advantage that came for free
from the storage design rather than from planner work: Neo4j must insert
`Eager` pipeline breakers to preserve *as-if-sequential* semantics, and those
breakers are one of the best-known causes of runaway memory in production
Cypher. GoGraph's immutable-snapshot reads make almost all of that
unnecessary, and the TCK holds at 3897/3897 without it.

---

## 2. Measured, unexploited headroom

Every item here is a case where GoGraph **already owns the data structure that
solves the problem** and the planner does not reach for it. That is what makes
them cheap relative to their value, and it is the direct answer to the
question the evaluation was asked.

### 2.1 `STARTS WITH` does not reach the sorted string index — measured 283×

A prefix predicate is a range predicate: `s STARTS WITH 'abc'` selects exactly
`s >= 'abc' AND s < 'abd'`. GoGraph has a sorted string range index
(`exec.StringRangeIndex`) and a shipped, exact-count-gated range-seek
peephole. The prefix form does not use it.

Confirmed by `Explain` on identical 100-row results over 50 000 `:EvPerson`
nodes with a btree index on `name`:

```
STARTS WITH "name002"                        range-equivalent
──────────────────────                       ────────────────
Selection                                    Selection
└─ NodeByLabelScan [p:EvPerson]              └─ NodeByIndexRangeScan
      (est. rows=50000, exact)                     (est. rows=100, exact)
```

| Form | sec/op | B/op | allocs/op |
|---|---|---|---|
| `STARTS WITH "name002"` | 11.56 ms | 2 016 008 | 149 816 |
| `>= "name002" AND < "name003"` | **40.89 µs** | **16 668** | **545** |
| **Ratio** | **283×** | **121×** | **275×** |

This needs no new statistics and no new gate. The index yields an *exact*
in-range count, which is the precondition the existing range-seek peephole
already demands — so the rewrite lands inside a proven no-regression frame.
Neo4j needs a dedicated `TEXT` index for this; GoGraph's existing btree is
already the right structure.

Note the scope boundary: `ENDS WITH` and `CONTAINS` are **not** range
predicates and get no such rewrite. Only the prefix case is sound.

### 2.2 Multi-label conjunctions scan-and-filter instead of intersecting bitmaps

`graph/index/label` stores each label as a Roaring bitmap and **already
implements `Index.Intersect(labels ...uint32)`** — a container-wise AND with an
early empty-exit. The Cypher planner calls it with exactly one label
(`cypher/api.go:4813`). Multi-label patterns instead take the P1 path: scan the
minimum-cardinality label and re-check the rest in a residual row `Filter`.

P1 is optimal when one label is small. It is *powerless* when both labels are
large and their intersection is tiny — which is the selective case users
actually write. With `|LabA| = |LabB| = 100 000` and `|LabA ∩ LabB| = 100`:

```
MATCH (n:LabA:LabB) RETURN count(n)
└─ EagerAggregation
   └─ Selection
      └─ NodeByLabelScan [n:LabA] (est. rows=100000, exact)
```

The planner knows *exactly* that it is about to scan 100 000 rows to return
100, and has no operator that can do better.

| Path | sec/op | B/op | allocs/op |
|---|---|---|---|
| Shipped: min-label scan + residual Filter | 18.60 ms | 811 961 | 99 825 |
| Access-path floor: one Roaring AND + count | 2.29 µs | 8 880 | **17** |

**Honest reading of these numbers.** The 8127× ratio compares an end-to-end
engine query against a bare access path, so it overstates the deliverable
win — the engine's fixed parse/plan/result overhead does not disappear. The
structural signal is the **99 825 allocations versus 17**: the engine
materialises ~100 000 rows to produce 100. Section 2.1 establishes the
realistic floor for a 100-row answer through the full engine at ≈41 µs, so the
achievable end-to-end gain here is a **few hundred ×**, not four orders of
magnitude.

Neither incumbent can do this: Neo4j's `LOOKUP` index gives a token scan then
filters, and Memgraph's `ScanAllByLabel` then filters. Set-at-a-time label
intersection over compressed bitmaps is available to GoGraph specifically
because it already stores labels that way.

The same lever extends to conjunctive *property* predicates, because
`RangeBitmap(lo, hi)` also returns a Roaring bitmap: `WHERE n.a > 1 AND n.b <
9` could be one AND of two index bitmaps. That is a strictly better answer than
Memgraph's composite index for GoGraph, because it needs **no new index type** —
any two single-property indexes compose.

Design caveat that must be respected: `Intersect` clones the first bitmap, so
the AND is not free when the intersection is nearly as large as the smaller
label. The gate is available and exact — Roaring can compute the intersection
cardinality before materialising — which fits GoGraph's established
exact-or-veto pattern.

### 2.3 A bound destination expands the whole adjacency — measured quadratic in degree

When a pattern's destination variable is already bound (cycle closing,
triangles, mutual-relationship detection), `cypher/ir/match.go` emits an
`Expand` to a synthetic column plus a `Selection` equating it with the bound
variable. Neo4j plans `Expand(Into)` here, which probes for the edge instead of
enumerating the neighbourhood. GoGraph has no such operator — confirmed by
`Explain`:

```
MATCH (a:CyN)-[:F]->(b:CyN)-[:F]->(a) RETURN count(*)
└─ Selection                                 ← b's destination == a, checked here
   └─ Expand (b)-[:F]->(__anon_2_to_a)       ← enumerates ALL of b's adjacency
      └─ Selection
         └─ Expand (a)-[:F]->(b) (est. rows=800, exact)
            └─ NodeByLabelScan [a:CyN] (est. rows=200, exact)
```

Measured on 20 000 nodes at two out-degrees:

| Out-degree | sec/op | B/op | allocs/op |
|---|---|---|---|
| 8 | 625.8 ms | 222 MB | 9.68 M |
| 64 | 41.68 s | 12.80 GB | 577.9 M |
| **Growth for 8× degree** | **66.6×** | **57.6×** | **59.7×** |

An 8× increase in degree costs **66.6×** the time. The empirical growth
exponent is `log(66.6)/log(8) = 2.02` — the second hop is **Θ(degree²)** where a
probe-based plan is `Θ(degree · log degree)`, exponent ≈1.1. This is a
performance cliff on one of the most common shapes in graph querying, and at
degree 64 it costs 41.7 seconds and 12.8 GB of allocation on a graph of only
1.28 M edges.

### 2.4 The enabling constraint: adjacency is not destination-sorted

> **⚠️ SUPERSEDED — 2026-07-29 (rmp #2139, delivered #2141/#2142/#2143,
> measured #2145).** The probe-cost table in this section is **refuted**. Its
> linear-scan column is roughly 6.5× too fast and, at degree 4096, describes a
> rate no Go scan can physically reach. The crossover is **≈16, not ≈64**, and
> the win at degree 4096 is **6.0× (hit) / 10.9× (miss), not 30.9×**.
>
> The corrected calibration is
> [`design-degree-adaptive-adjacency.md` §2.2](design-degree-adaptive-adjacency.md);
> the end-to-end result is
> [`benchmarks/csr-neighbour-ordering-2026-07-29.md`](benchmarks/csr-neighbour-ordering-2026-07-29.md).
> **Do not carry the figures below forward as current.**
>
> This section's *recommendation* is also superseded. It proposed a
> degree-adaptive adjacency; what shipped is an **unconditional** ordering of the
> CSR snapshot, with the adjacency left unordered on three structural grounds
> (`graph/csr/order.go`, and the `adjEntry` comment in `graph/adjlist/adjlist.go`
> — cited below by symbol rather than by line, because that comment has since been
> rewritten and the line number in this section no longer points at it).
>
> The *leverage* analysis of §2.4 in the design document — the `Σd²` costFrac
> table — is a **different** table and is **confirmed**: it now reproduces to
> within 0.01 percentage points under
> `TestDistribution_Reproduces24Table` (soak layer). Only the probe-cost column
> here is refuted.
>
> **Why this was not caught earlier, and the lesson.** The harness behind the
> table below was retained outside the repository and so **cannot be re-run**. A
> load-bearing measurement that cannot be reproduced is not evidence. The
> replacement measurements are committed as permanent benchmarks in
> [`bench/csrorder/`](../bench/csrorder) precisely so this cannot recur.

Sections 2.3 and the shipped OUT-only restriction on the anchor-swap peephole
have one common root, documented on `adjEntry` in `graph/adjlist/adjlist.go`
(at line 241 when this audit was written):

> Adjacency is kept unsorted; HasEdge does a linear scan. For the typical low
> average degree of property graphs (4-16) the branch-prediction-friendly
> linear scan beats sorted binary search.

CSR inherits this: `graph/csr/csr.go` sorts edges *by source*, and within a
source "the order is the caller's". Consequently `Expand.lookupFwdEdgePos` is a
linear scan, which is why P3 had to veto reverse-introducing anchor swaps as
not soundly costable, and why `MergeRelationship`'s `HasEdge` check is O(degree)
on the **write** path too.

The documented rationale was tested rather than assumed, over a realistic
neighbour range, worst-case target slot:

**⚠️ REFUTED — every figure in this table is superseded. Retained only as the
record of what was claimed.**

| Out-degree | Linear probe | Binary probe | Winner | Corrected (§2.2 of the design doc) |
|---|---|---|---|---|
| 8 | ~~0.659 ns~~ | ~~1.865 ns~~ | ~~linear, 2.8×~~ | 6.59 / 11.36 ns → linear 1.7× |
| 64 | ~~2.99 ns~~ | ~~2.98 ns~~ | ~~parity — crossover~~ | 86.52 / 42.47 ns → **binary 2.04×**; parity is at **degree 16** |
| 512 | ~~20.5 ns~~ | ~~4.11 ns~~ | ~~binary, 5.0×~~ | 225.99 / 87.83 ns → binary 2.57× |
| 4096 | ~~164 ns~~ | ~~5.31 ns~~ | ~~binary, 30.9×~~ | 797.27 / 132.07 ns → binary **6.04×** |

The table was refuted three independent ways: directly (a linear scan measures
0.268–0.348 ns/element, not the 0.040 implied by 164 ns at degree 4096); by a
floor argument (a branch-free unrolled accumulate measures 0.164 ns/element, so
the claim is 4.1× faster than the fastest a Go scan can run); and by its own
internal inconsistency (its per-element cost *falls* as degree grows, which a
longer scan cannot do).

~~**The comment is correct, and it is also incomplete.**~~ The comment was
correct — and its stated band of 4–16 is precisely what survived
re-measurement, since the crossover is ≈16. What the comment lacked was the
measured crossover value and the reasons the adjacency stays unordered; both
have since been written into it.

~~So the correct design is **not** "sort the adjacency". It is a
**degree-adaptive representation**~~ — **superseded.** A degree-adaptive
representation was designed under #2139 and **rejected**: a per-source "is
sorted" flag costs a branch at every probe site, imposes a correctness
obligation that every site consult it, and needs a promotion rule that is
incompatible with recovery determinism. What shipped instead is an
**unconditional** ordering of every CSR the build produces
(`graph/csr.OrderRuns`), which is possible at zero write-path cost because all
of the read-path probes traverse the CSR snapshot rather than the adjacency.

Of the four items this section expected to unlock, **three are enabled by the
CSR ordering and the fourth is deliberately not**:

- ✅ `Expand(Into)` for bound destinations (§2.3);
- ✅ a **symmetric** anchor swap, lifting the shipped OUT-only restriction, since
  `lookupFwdEdgePos` becomes soundly costable;
- ✅ sorted-merge intersection — the primitive §4 depends on;
- ❌ an O(log d) `HasEdge` for `MERGE` on the write path. **Not delivered, and
  not planned.** This is the only one of the four that needs the *adjacency*
  ordered rather than the CSR, and that is the half dropped as #2140
  (superseded) on the three structural grounds above. `HasEdge` remains an O(d)
  scan.

The three enabled items are *enabled*, which is not the same as shipped: the
O(log d) probe they rest on landed as #2142, and each peephole that consumes it
is its own change.

---

## 3. Where the incumbents still win

Stated plainly, because an evaluation that only lists strengths is not an
evaluation.

| Gap | Neo4j | Memgraph | Assessment |
|---|---|---|---|
| Join/expand ordering beyond a single edge | Cost-based IDP over the whole pattern | Rule-generate + cost-select | **Structural, and deliberate.** Under the absolute no-regression mandate every ≥2-join plan is vetoed, because the second intermediate cardinality is a correlated degree sum, not a marginal product. Closing this requires accepting margin-gated (non-absolute) plans — a mandate change, i.e. the user's call, not the planner's. |
| Anchor choice on skewed data with no index | Cost-driven, reverses freely | Cost-driven | Partly closed by P3's count-store-gated swap; still OUT-only until §2.4 lands. |
| Query hints | `USING INDEX` / `INDEX SEEK` / `SCAN` / `JOIN ON` | `USING INDEX` | Not implemented — no grammar rule for `USING`. Tracked as a known divergence. Low value while plans are stable and peepholes are exact, but it is the standard operator escape hatch. |
| `ORDER BY` served from index order | Yes | Yes | Not available: `RangeBitmap` returns a NodeID-ordered bitmap, which discards index order by construction. A consequence of the bitmap design, not an oversight — recovering it needs a cursor-style covering scan, so it trades against §2.2's set-at-a-time win. Worth an explicit decision, not a silent one. |
| Variable-length `DISTINCT` pruning | `PruningVarLengthExpand` | — | Absent. `MATCH (a)-[*1..3]->(b) RETURN DISTINCT b` enumerates all paths, then deduplicates. |
| Relationship-property / edge-type index | Yes | `CREATE EDGE INDEX` | Absent. |
| Triadic selection | Yes | — | Absent; largely subsumed by §2.3 + §4. |

The first row is the honest headline: **GoGraph's ordering gap is not a defect,
it is the price of the no-regression guarantee.** The incumbents buy better
ordering by accepting that a plan can be worse than written order when their
estimates are wrong. That is a legitimate trade, and reversing it is a decision
about mandates rather than a piece of engineering.

---

## 4. The asymmetric opportunity: cyclic patterns

The strongest available answer to *what can GoGraph do better than both* is a
capability **neither incumbent has at all**.

Neo4j and Memgraph both execute exclusively with binary joins in left-deep
pipelines. For **cyclic** patterns — triangles, squares, any closed motif —
binary-join plans are *provably* suboptimal: no binary-join plan can meet the
AGM bound (Atserias–Grohe–Marx, FOCS'08). Triangle enumeration over `m` edges
costs `Θ(m²)` for a binary-join engine against the `Θ(m^1.5)` worst-case-optimal
bound, achieved only by worst-case-optimal joins (Ngo et al., PODS'12;
Veldhuizen's Leapfrog Triejoin, ICDT'14).

§2.3 measured GoGraph currently paying that quadratic price and more. But the
WCOJ primitive is **sorted-set intersection**, and §2.4 shows GoGraph is one
representation change away from having it — over flat, cache-resident CSR
arrays, which is a far better substrate for leapfrog intersection than a
record-store engine.

This is the one place where GoGraph could hold an **asymptotic** advantage over
Neo4j and Memgraph rather than a constant-factor one. Two honest caveats:

- The prior design analysis correctly **vetoed cyclic patterns** for the
  reordering peepholes precisely because they sit at this boundary. That veto
  was the right call for a binary-join reorderer; it is also the signpost
  saying a different operator is required.
- This is a genuine new operator with real cost and risk, not a peephole. It
  should not be started before the cheap wins in §2 are taken.

---

## 5. Recommendation

Ordered by measured value per unit of risk. Every item is additive and sits
inside the existing exact-or-veto frame; none requires relaxing the
no-regression mandate.

| # | Change | Measured basis | Risk |
|---|---|---|---|
| 1 | `STARTS WITH` → prefix range seek | **283×** time, 121× memory, 275× allocs (§2.1) | **Lowest.** Reuses the shipped range-seek peephole and its exact-count gate verbatim. Prefix only. |
| 2 | Multi-label conjunction → Roaring AND | 99 825 → 17 allocs; a few hundred × end-to-end (§2.2) | Low. `Index.Intersect` already exists and is tested. Needs an exact intersection-cardinality gate. |
| 3 | Degree-adaptive sorted adjacency above the measured crossover | crossover at degree ≈64; up to **30.9×** probe win (§2.4) | Medium — touches the storage layer, so ACID-sensitive and must prove write-path neutrality. Preserves the documented low-degree win by construction. |
| 4 | `Expand(Into)` for bound destinations | **Θ(d²) → Θ(d log d)**, exponent 2.02 measured (§2.3) | Low **once 3 lands**; depends on it. |
| 5 | Widen anchor swap to symmetric | lifts P3's deliberate OUT-only restriction | Low once 3 lands; re-uses the shipped two-sided gate. |
| 6 | Conjunctive property predicates → bitmap AND | same lever as 2 | Low. Removes the need for a composite index type. |
| 7 | Worst-case-optimal join for cyclic patterns | `Θ(m²) → Θ(m^1.5)`; **neither incumbent has this** (§4) | Highest — a new operator. Sequence last; depends on 3. |

Items 1 and 2 are pure planner peepholes reusing shipped machinery. Item 3 is
the keystone: it is the only storage-layer change, and items 4, 5 and 7 all
depend on it. That dependency structure — one medium-risk enabler unlocking
four wins, three of them measured — is the same shape as the count-store's role
in P2→P3, which delivered.

Two decisions belong to the user and are flagged rather than assumed:

- whether to keep the **absolute** no-regression mandate (§3, row 1), which is
  what bounds ordering quality;
- whether index-order `ORDER BY` (§3) is worth trading against the
  set-at-a-time bitmap design in §2.2.

## Reproducing the measurements

The harness is not committed — these numbers belong to the evaluation, and each
implementation task should land its own permanent benchmark per the standard
workflow. Fixtures, exactly as measured:

- **§2.1** — 50 000 `:EvPerson` nodes, `name = "name%05d"`, btree index on
  `name`; `STARTS WITH "name002"` vs `>= "name002" AND < "name003"`; both
  return 100 rows (asserted before timing).
- **§2.2** — `|LabA| = |LabB| = 100 000`, overlap 100; `MATCH (n:LabA:LabB)
  RETURN count(n)` vs `label.Index.Intersect(LabA, LabB).GetCardinality()`;
  equality of the two answers asserted before timing.
- **§2.3** — 20 000 `:CyN` nodes, ring-structured out-degree 8 and 64;
  `MATCH (a:CyN)-[:F]->(b:CyN)-[:F]->(a) RETURN count(*)`.
- **§2.4** — neighbour slice of the stated degree, worst-case target slot;
  linear scan vs branchless binary search.

Apple M4, `darwin/arm64`, `-benchmem`, `-count=6` (§2.1, §2.2) and `-count=5`
(§2.3, §2.4). A copy of the harness is retained outside the repository.
