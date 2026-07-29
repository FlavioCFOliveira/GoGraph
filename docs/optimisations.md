# Optimisation Log

This document tracks every measured optimisation applied across
sprints, with the benchstat-style before / after numbers that
justify the change. Every entry is the artefact of a sprint task
or a one-off fix that landed in main.

## Initial optimisation pass

### graph/adjlist — switched to copy-on-write with linear scan

Replaced the original sorted-with-binary-search adjacency list
(map-of-pointers, binary-search lookup) with the copy-on-write
unsorted adjacency layout currently in main.

| Operation                            | Before        | After         | Result |
|--------------------------------------|---------------|---------------|--------|
| HasEdge (hot cache, 1K nodes)        | 281 ns/op     | 49.3 ns/op    | 5.7x   |
| HasEdge (cold, 1M nodes / 4M edges)  | 281 ns/op     | 175 ns/op     | DRAM floor reached |
| AddEdge (1M nodes)                   | 492 ns/op     | 423 ns/op     | modest |

The hot-cache HasEdge result matches the documented AC (<50 ns).
The AddEdge cost is dominated by the 2-slice copy on every write
and is a known future optimisation target (delta-log or in-place
atomic append).

### search/bfs — wavefront frontier

Replaced the accumulating-queue BFS with the per-level wavefront
swap.

| Operation                | Before                       | After                       |
|--------------------------|------------------------------|-----------------------------|
| BFS 10^7-node chain      | 828 MB peak, 55 allocs       | 1.25 MB peak, 0 allocs/op   |
| BFS time on 10^7 chain   | 89 ms                        | 38 ms (post-warmup)         |

Acceptance criterion (<200 MB peak heap) achieved with 660x
margin.

### store/csrfile — zero-copy mmap reinterpretation

`csrfile.Reinterpret[T]` retypes a byte slice as `[]T` without
copying.

| Variant                     | ns/op    | allocs/op |
|-----------------------------|----------|-----------|
| Reinterpret[uint64] 1024 vs | 1.31     | 0         |
| naive copy (1024 uint64)    | ~5800    | 1         |

## v0.3.1 performance cycle (2026-06-14, tasks #1497–#1525)

The per-change record, with the full benchstat output and guard-band
confirmation for each step, lives in
[benchmarks/history/LEDGER.md](benchmarks/history/LEDGER.md) (rows
0006–0016). Headline measured wins:

| Change | Task | Fixture | Result |
|--------|------|---------|--------|
| Group commit / WAL fsync coalescing | #1507 | `BenchmarkCommitConcurrent` (256 g) | −99.16 % (≈ 118× throughput), single-thread flat |
| Parallel pull-formulation PageRank over reverse-CSR | #1513 | `PageRank_PowerLaw50K`, 100K/3.2M | 1.68–1.77× (2.40× SpMV kernel), bit-identical |
| Range-predicate B+tree index seek | #1505 | `BenchmarkRangeSeekSelective` | −99.11 % time (≈ 114×), −98.95 % B/op |
| Hash join for disconnected equi-joins | #1506 | `BenchmarkHashJoinDisconnectedEquiJoin` | ≈ 93× faster, ≈ 95× less memory |
| Real B+ tree replacing the sorted-array index | #1514 | — | range property index is now a real B+ tree |
| Column-oriented (SoA) result rows | #1499 | `cypher_ldbc` IC1 | −32.4 % time / −60.9 % B/op / −25.6 % allocs |
| Lock-free copy-on-write metadata name registry | #1503 | `BenchmarkNodeMetadataReadParallel` (8-way) | −81.57 % time |

Every change is benchstat-gated against the `f6f8c7a` baseline (ledger
row 0006); the curated search guard band (Dijkstra / BFS / Brandes) stayed
flat, TCK held at 3897/3897, and ACID was preserved (the group-commit
write path was storage-engine-auditor-certified).

## Sprint 305 — min-cardinality multi-label anchor scan (F1b, 2026-07-23)

When a node pattern carries several labels (`MATCH (n:A:B:C)`), the
planner now scans the label with the smallest **exact** bitmap
cardinality and keeps the remaining labels as a residual `Filter`,
instead of always anchoring on the first syntactic label. It is a
build-time peephole (the same layer as the index-seek rewrite), so the
logical plan and the result multiset are unchanged: a label conjunction
is a commutative AND and `min|Lᵢ| ≤ |L0|`, so the plan never does more
work than the default. `EXPLAIN`/`PROFILE` render the chosen label on
the `NodeByLabelScan`. Gated by `EngineOptions.DisableMinLabelScan`
(feature enabled by default).

| Change | Task | Fixture | Result |
|--------|------|---------|--------|
| Min-cardinality multi-label anchor scan | #2077 | `BenchmarkMinLabelScanSelective` (large `:Common` + small `:Rare`) | ≈82× faster, ≈22× less memory, ≈122× fewer allocs (223.6µs vs 18.4ms) |

Inert on non-multi-label queries (the peephole shape is absent), so the
curated suite is unaffected; the differential harness (#2078) proves the
ON/OFF result multisets are byte-identical, and the estimate-provenance
veto (#2076) keeps the plan default whenever a count is not trustworthy.
Full record: [benchmarks/history/LEDGER.md](benchmarks/history/LEDGER.md)
row 0020. TCK held at 3897/3897, -race clean.

## Sprint 306 — exact relationship count-store (F5a, 2026-07-23)

The planner gains an exact relationship statistic — the enabler for the
count-store-gated join reordering in P3. A derived, non-durable count-store
keeps exact `E(relType)`, `D(label,relType,dir)` and `T(labelA,relType,labelB)`
counts (reusing the label index for `N(label)`), updated O(delta) on the
commit fan-out via a `CountBuffer` under the `visMu` barrier, and recomputed
O(V+E) at reopen (no WAL op, no checkpoint component). Node relabel keeps the
OUT side exact within a per-commit budget and marks the un-enumerable IN side
stale, self-healing at reopen; a stale cell yields an `EstFallback` veto,
never a wrong exact.

| Change | Task | Fixture | Result |
|--------|------|---------|--------|
| Exact relationship count-store | #2082 | `BenchmarkEngWriteAutocommit` (n=10) | allocs/op 34 → 34 (all samples equal); B/op +2.36% fixed; sec/op +0.51% noise |

The count-store is inert until P3 wires the join reorder. Write-path
neutrality (the #2051 failure mode) is proven by the identical allocs/op —
LEDGER row 0021. Observable via `internal/metrics` counters
`cypher.countstore.{delta.applied,lookup,lookup.veto,relabel.dirtied}`, the
`cypher.countstore.recompute` latency, and `Engine.CountStoreCells()`. TCK
held at 3897/3897, -race clean; reopen-parity exact.

## Sprint 307 — count-store-gated reordering (P3, 2026-07-24)

Two build-time reordering peepholes, each gated by the exact count-store and the
order-safety predicate so they deviate from the written order only when provably
result-identical and never slower:

- **Disjoint-component ordering** — for a nested-loop join of disjoint
  single-scan components (no equi-join), build the smaller exact node count as
  the outer side.
- **Single-edge anchor-swap** — for `(a:A)-[:R]->(b:B)`, anchor on the endpoint
  that minimises examined edges via the count-store degree `D(label,relType,dir)`;
  cost `c_s·N + c_e·D`. OUT-only: it flips a written `DirIn` expand to `DirOut`;
  reverse-introducing swaps are vetoed because a `DirIn` traversal costs work
  proportional to the source out-degree (a forward-range scan), invisible to
  aggregate `D`.

| Change | Task | Fixture | Result |
|--------|------|---------|--------|
| Single-edge anchor-swap (OUT-only) | #2090 | `BenchmarkAnchorSwapHub` | −92% time (12.7×), −99.5% allocs |
| Disjoint-component ordering | #2091 | `BenchmarkJoinReorderDisjoint` | −67% time (3.1×), −83% B/op |

Both are gated by two-sided trustworthiness (candidate and baseline exact and
non-dirty, sampled from one snapshot) and the `SuppressReorder` order-safety
predicate; a relabel-dirtied count vetoes back to the written order; the curated
suite is byte-identical (the peepholes never fire there). The query plan now
renders the chosen scan label and expand direction so the reorder is visible.
TCK held at 3897/3897, -race clean; LEDGER row 0022. Design:
[reordering-design.md](reordering-design.md).

## Sprint 308 — planner statistics + cardinality estimates (P4/F5b, 2026-07-24)

Off-write-path, best-effort statistics that make the planner observable without
regressing writes:

- **HyperLogLog NDV** (m=4096, ~1.6% error), **equi-depth histograms** (B=256,
  ≤1/B error, MCV-spike isolation), and **exact top-k MCV** — built by an
  explicit `RefreshStatistics` scan, generation-stamped and staleness-gated. NDV
  is never sampled (provably impossible to bound — Charikar et al. PODS'00).
- **Consumed by EXPLAIN/PROFILE:** each operator is annotated with an estimated
  row count and its provenance (exact / stats / heuristic), drawn from the exact
  count-store (`N`, `E`, `D`) and the new statistics. Display-only — no execution
  or plan-choice change (a differential test proves results identical with/without
  statistics populated).
- **The range-index seek stays exact-count-gated** (already optimal). estStats was
  proven *not* to widen it — the index yields an exact in-range count whenever a
  seek is possible — so the statistics' role is observability plus a documented
  foundation for a future margin-gated (non-absolute) mode.

| Change | Task | Fixture | Result |
|--------|------|---------|--------|
| Statistics maintenance (lazy collector) | #2101 | `BenchmarkEngWriteAutocommit` | write path flat vs pre-stats (B/op p=0.18, allocs 34→34); LEDGER row 0023 |

Zero write-regression: the collector is **lazy** (nil until `RefreshStatistics`),
so unused-statistics writes are byte-identical to pre-statistics; with statistics
active the maintenance is an O(1) atomic Δ per tracked property, zero-alloc.
Honest limit: single-column range selectivity promotes to estStats; equality
stays exact-MCV-or-heuristic (uniformity error is unbounded under skew); multi-join
independence is never promoted. TCK 3897/3897 throughout. Design:
[statistics-design.md](statistics-design.md).

## Sprint 309 — columnar/vectorized execution deepening (P5/F3, 2026-07-24)

Extends the columnar late-materialization runtime (the differentiator neither
Neo4j nor Memgraph has — Neo4j's morsel is a row batch, Memgraph is row-at-a-time)
from projection to the operators that dominate analytic Cypher. Governing
principle (the chunk-pipeline rule): boxing is removed only on the contiguous
`ChunkProducer` suffix measured from the sink, so each columnar operator is a
drop-in with a row-mode fallback, wired only for its qualifying shape, and
differential-tested byte-identical columnar-ON vs row-OFF.

| Change | Task | Fixture | Result |
|--------|------|---------|--------|
| Columnar aggregation (SoA scatter-add) | #2104 | `BenchmarkColumnarAgg` (1M-row group-by) | allocs −99.97% (3.15M→803), −43% time |
| Expand→ChunkProducer + filter-over-traversal | #2106 | `BenchmarkExpandFilter` (~64k edges) | allocs ≈7200× fewer (181k→25), B/op ~4.1× lower |
| Columnar hash-join (late materialization) | #2105 | `BenchmarkHashJoinDisconnectedEquiJoin` | allocs −22.4%, B/op −0.84%, sec/op −5.7% |

The chunk pipeline now stays unboxed end-to-end through scan → expand → filter →
aggregation. Exact int/float semantics preserved (CIP2016-06-14: exact int64
`SUM`, `int 1 ≡ float 1.0` grouping/join, `int 2^53+1 ≠ float 2^53`). Reduced
scope: traversal itself and entity/`Path` columns stay boxed (pointer-rich, not
scan-heavy). TCK 3897/3897 throughout. LEDGER row 0024. Design:
[columnar-deepening-design.md](columnar-deepening-design.md). Extends the columnar
value-model epic #1704.

## Sprint 310 — automatic intra-query parallelism broadening (P6/F4, 2026-07-24)

Extends the automatic (no-licence, no opt-in) morsel-parallelism from scan/count
to min/max aggregation, and pushes the all-node count to O(1). Unlike Neo4j's and
Memgraph's parallel runtimes (Enterprise + opt-in + read-only), these engage
automatically above `parallelScanThreshold` and are governed (`ParallelGovernor`
bounds total workers near GOMAXPROCS across concurrent queries).

| Change | Task | Fixture | Result |
|--------|------|---------|--------|
| Parallel min/max aggregation (position-carrying combine) | #2111 | `BenchmarkParallelAggregate` (single-query, large graph) | ~2.83× (conc=1); conc=64 (saturated) parity |
| Serial O(1) count-pushdown | #2113 | `BenchmarkCountPushdown` | −99.99% (O(1) vs O(N), ≈9700×; 399.6k→30 allocs) |

Determinism: min/max use a position-carrying combine byte-identical to serial for
every tie representation (int/float, ±0, NaN); float `sum`/`avg`, int `sum`,
`collect`/`percentile`/`stdev` stay serial (no byte-identical combine under the
multiset + value-representation determinism obligation). Workers reuse
`ParallelGovernor`, are goleak-clean, context-cancellable and byte-budget-bounded;
a `budget==1` inline-serial short-circuit (#2115) keeps the saturated regime
no-regression (conc=64 parity). Intra-query parallelism is idle-core-bound (the
win is at low concurrency; the governor throttles under load). **Parallel Expand
(#2112) was benched and DEFERRED** — single-query 4–5× but idle-core-bound, so no
win under GoGraph's high-concurrency production regime (0.88× at conc=8). Load-test
at conc 1/8/64: LEDGER row 0025. A pre-existing `ParallelGovernor` transition-zone
over-subscription residual is tracked (#2125); per-operator engagement counters
are tracked (#2123). TCK 3897/3897 throughout.

## Sprint 311 — index-backed string prefix predicate (R2-P1, 2026-07-28)

`n.p STARTS WITH 'x'` now reaches the sorted string B-tree instead of
scanning the label and refiltering every row. A prefix **is** a range:
the matching keys are exactly the half-open interval `[x, succ(x))` of the
byte-lexicographic order the B-tree is laid out in, so the predicate is
served by the shipped range-seek peephole with its **exact-count gate
reused verbatim** — no new statistic, no new gate, no relaxation of the
no-regression mandate. The original predicate is always retained as the
residual `Filter`, so the seek can only narrow what the filter examines.

`succ(x)` is the **byte** successor (the last byte below `0xFF`,
incremented), not a code-point increment: the index orders keys by
`cmp.Compare` and `STARTS WITH` is `strings.HasPrefix`, so both sides
already share one byte basis. It also always exists for a non-empty
prefix that is not all-`0xFF` — a code-point increment has none at
U+10FFFF — and is tighter. Where no finite successor exists (the empty
prefix, an all-`0xFF` prefix) the upper bound is left unbounded and the
scan runs open-ended. Gated by `EngineOptions.DisablePrefixIndexSeek`
(feature enabled by default), a **separate** knob from
`DisableRangeIndexSeek` so the differential can vary one thing.

| Change | Task | Fixture | Result |
|--------|------|---------|--------|
| `STARTS WITH` → prefix range seek | #2127 | `BenchmarkPrefixSeek` (50 000 nodes, btree on `name`, 100-row result, `-count=10`) | **385.2× faster** (11812.61 µs → 30.67 µs), **90.7× less memory** (1977.16 KiB → 21.79 KiB), **407.1× fewer allocs** (149 829 → 368) |

Scope is prefix-only, and the boundary is a correctness boundary rather
than a performance one: `NOT … STARTS WITH …` selects the *complement* of
the interval, so rewriting it would be a non-superset. `ENDS WITH` and
`CONTAINS` are excluded by proof — their match set is not an interval of
the byte order (`"ax" < "b" < "bx"`), so the narrowest sound interval is
the whole index: not unsound, but useless. Negation, disjunction, the
mirrored `'lit' STARTS WITH n.p`, a parameterised prefix, an unindexed
property, and every gate veto (empty prefix, broad prefix, zero-match
prefix) all keep the label scan, each pinned by a regression test.

Inert where the shape is absent: allocations and bytes are
**byte-identical on all 23 curated benchmarks** across the A/B commit
boundary — the column that moved in the row-0023 malloc-size-class
regression this change could have repeated by adding a field to
`Engine`. Correctness is gated by a differential (rewrite ON vs OFF)
**plus an absolute Go oracle** computed from the fixture with
`strings.HasPrefix`, **plus** plan-difference assertions, **plus** a
`rapid` property that fails if the seek never fires; the harness itself
was validated by injecting three mutations and confirming each fails.
Design and proofs:
[design-prefix-range-seek.md](design-prefix-range-seek.md). Full record:
[benchmarks/prefix-range-seek-2026-07-28.md](benchmarks/prefix-range-seek-2026-07-28.md)
and [benchmarks/history/LEDGER.md](benchmarks/history/LEDGER.md) rows
0026–0027. TCK held at 3897/3897, -race clean.

Two follow-ups were measured and **filed rather than fixed**: a range
predicate with a third conjunct gives up the seek entirely (92×, #2245),
and `STARTS WITH` is not `ColumnarFilter`-eligible, so a gate-vetoed
prefix still pays row-mode filtering (~3.3×, #2246).

## Sprint 312 — set-at-a-time bitmap intersection (R2-P2, 2026-07-28)

`MATCH (n:A:B)` is a set intersection, and it is now answered as one. Labels
were already stored as Roaring bitmaps and `label.Index.Intersect` — a
container-wise AND with an early empty-exit — was already implemented and
tested; the planner just called it with a single label. The conjunction is now
one k-way AND, and the residual label `Filter` is **dropped**, because the
intersected bitmap already encodes the conjunction.

Dropping that filter is the win, and it is sound because the label index was
**measured** to be maintained on both delete and relabel, so the bitmap is
authoritative for membership *and* liveness. It also adds no exposure: a plain
single-label `MATCH (n:L)` already iterates a bitmap materialised at `Init`
with no residual filter at all.

The AND is ordered **smallest-first**, because `Intersect` clones its first
argument — worth 6.0× in time and 7.5× in memory on a skewed pair, using the
exact counts the min-label peephole (#2077) already computes.

| Change | Task | Fixture | Result |
|--------|------|---------|--------|
| Multi-label conjunction → Roaring AND | #2133 | `BenchmarkLabelIntersect`, `\|LabA\|=\|LabB\|=100 000`, `\|∩\|=100`, end-to-end, `-count=10` | `RETURN count(n)` **2 909× faster** (22 312.42 µs → 7.669 µs), **988× fewer allocs** (99 822 → 101); `RETURN n.k` **267× faster** |
| Conjunctive indexed properties → bitmap AND | #2134 | 20 000 `:Doc` nodes, three btree indexes | composes two single-property indexes, and across index kinds (numeric + string) |

**The gate is a strict reduction in rows scanned**, `|L₁ ∩ … ∩ L_k| < min|Lᵢ|`,
decided on an exact count that allocates nothing
(`label.Index.IntersectCardinality`). It is not a tuned ratio: a ratio was tried
first and declined cases the cost model says must be served. When the
intersection equals the smallest label there are no rows left to remove, so the
shape is left to the columnar filter chain — symmetric with the rule that
recogniser already applies to the min-label re-anchor, that a rewrite may
pre-empt another only when it removes **rows** rather than a constant factor per
row. Every veto falls through to the shipped min-label plan, never to something
worse. An empty label short-circuits the whole conjunction.

For **properties** the discipline differs and the design said so before the code
was written: a label bitmap is *exact*, so its filter is dropped; a range bitmap
is a *superset* by construction (#F-EXEC1), so intersection is still sound —
supersets are closed under intersection — but the residual `Filter` is
**mandatory** there. That is what lets any two ordinary single-property indexes
answer `WHERE n.a > 1 AND n.b < 9`, which other engines need a dedicated
composite index type for, with no new index type and no new statistic.

Neither Neo4j nor Memgraph does set-at-a-time label intersection: Neo4j's
`LOOKUP` index gives a token scan then filters, and Memgraph's `ScanAllByLabel`
then filters.

Inert where the shape is absent: allocations are **flat (geomean +0.00%)** on
both `cypher_ldbc` and `cypher_alloc` across the A/B commit boundary — after
three rounds of fixing a probe that ran on every `Selection` and was not free
when it declined, two of whose causes were visible only under
`go build -gcflags=-m`. Correctness is gated by a differential, an absolute Go
oracle over label memberships, plan-difference assertions, a `rapid` property
that fails if the path never fires, and a race-free concurrent-relabelling
invariant; the harness was validated by mutation. Design and proofs:
[design-bitmap-intersection.md](design-bitmap-intersection.md). Full record:
[benchmarks/bitmap-intersection-2026-07-28.md](benchmarks/bitmap-intersection-2026-07-28.md)
and [benchmarks/history/LEDGER.md](benchmarks/history/LEDGER.md) rows 0028–0029.
TCK held at 3897/3897, -race clean.

## Sprint 313 — destination-ordered CSR neighbour runs (R2-P3, 2026-07-29)

Every CSR source's neighbour run is now ordered by the total key
`(destination, handle)`, so the executor's five forward-position membership
probes binary-search instead of scanning. Those probes fire on the **reverse and
undirected** expand path: each reverse slot must locate its corresponding
forward edge, which cost O(deg(dst)) in the destination's forward run.

Measured interleaved against the pre-sprint tree (n=10, every row p=0.000):

| out-degree | time | B/op |
|---:|---:|---:|
| 8 | −8.28% | −59.20% |
| 16 | −5.60% | −58.01% |
| 32 | −6.83% | −57.26% |
| 64 | −8.42% | −56.92% |
| 512 | **−36.75%** | −56.59% |
| 4096 | **−81.76%** | −56.56% |
| **Barabási–Albert power law** | **−7.37%** | **−58.47%** |
| RMAT scale=16 ef=16 | −28.69% | −58.39% |
| geomean | **−31.77%** | **−57.58%** |

**Quote the power-law row, not the geomean.** The sweep deliberately includes
out-degrees no real property graph reaches, and RMAT overstates the win by
**3.9×** — the trap `design-degree-adaptive-adjacency.md` §8 forbids
benchmarking into. So: a modest win on a realistic graph, a large win on
hub-heavy shapes, **no regression at any degree**.

The win is *attributable* because the three commits have different signatures.
The #2143 pair cache is degree-independent — `B/op` falls ~57% everywhere and
HEAD sits flat at 14.00 MiB where the baseline scaled 32–34 MiB — and is the
whole story at degrees 8–64. The ordering and the O(log d) probe are the extra
−28 percentage points at degree 512 and −73 at 4096.

**The write path pays for this, and the number is large.** Ordering makes a CSR
build **2.5×–34× more expensive** (+152% at degree 8 to +3363% at 4096;
`OrderRuns` alone 2.7 ms → 22.1 ms) and the checkpoint **+16.52%** geomean,
peaking at +30.82%. That trade is only favourable because #2143 moved the build
off the per-query path into an `Engine`-level cache keyed on `TopoGeneration`;
without it, this cost would be paid on every query. The checkpoint's percentage
is non-monotonic but its *absolute* delta rises monotonically (+10.9 ms →
+38.9 ms), exactly as O(Σ d log d) predicts.

Ordering is **unconditional**, not degree-adaptive. A per-source "is sorted" flag
would cost a branch at every probe site, oblige every site to consult it, and
need a promotion rule incompatible with recovery determinism. The **adjacency is
deliberately left unordered** on three structural grounds (no `AuxColumn`
permutation primitive; the zero-allocation tail append cannot survive an ordered
insert; a history-dependent representation lets a recovered graph diverge) — so
`HasEdge` remains an O(d) scan and the `MERGE` write-path win was dropped
(#2140, superseded).

Two evidence notes worth carrying forward. The sprint's motivating audit figure
was **refuted as physically unattainable** (it implied 0.040 ns/element against a
measured branch-free floor of 0.164), and the crossover is **≈16, not ≈64**. And
the first A/B measurement of this change was **invalid**: `bench-history.sh`
compares back-to-back, which on this hardware manufactured a spurious +2.12%
`cypher_ldbc` geomean regression that vanished entirely under interleaving — all
23 curated benchmarks are flat. A byte-identical control caught it. Full record:
[benchmarks/csr-neighbour-ordering-2026-07-29.md](benchmarks/csr-neighbour-ordering-2026-07-29.md)
and [benchmarks/history/LEDGER.md](benchmarks/history/LEDGER.md) rows 0030–0031.
TCK held at 3897/3897, `-race` clean.

## Workflow

Every future optimisation appends a row to the table above with
the benchstat numbers and the before/after summary in the commit
message that lands it. The `simplify` skill is the standard tool
for review-and-apply rounds.
