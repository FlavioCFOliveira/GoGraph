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

## Workflow

Every future optimisation appends a row to the table above with
the benchstat numbers and the before/after summary in the commit
message that lands it. The `simplify` skill is the standard tool
for review-and-apply rounds.
