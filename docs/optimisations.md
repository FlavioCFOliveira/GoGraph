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

## Workflow

Every future optimisation appends a row to the table above with
the benchstat numbers and the before/after summary in the commit
message that lands it. The `simplify` skill is the standard tool
for review-and-apply rounds.
