# Group-commit coalescing across the concurrency ladder

**rmp #2302 (MVCC A5) acceptance criterion 6 · sprint 334 · 2026-08-03**

The criterion: *a benchmark shows fsyncs per commit falls as concurrency rises.* This is the evidence,
and it exists to answer one question the contiguity change raised — whether moving a transaction's WAL
append into a single exclusive run (`wal.Writer.AppendRun`) cost the group commit that
`wal.Writer.SyncGroup` already performed.

It did not, and the reason is structural rather than lucky: `SyncGroup` coalesces on **Sync**, not on
Append. A run of appends followed by one sync is precisely the shape it already batched.

## Method

- Benchmark: `BenchmarkWriteScaling_StoreAPI`, `store/txn/write_scaling_bench_test.go`.
- Unit of work: one durable transaction (`txn.Store.Begin` → `Tx.AddEdge` → `Tx.Commit`) per
  operation, on keys unique per operation so no two writers contend on a node.
- `commits/fsync` is `b.N` divided by the rise in `wal.Stats().Syncs` across the timed loop — the mean
  group size, read from the writer's own counter rather than inferred.
- Machine: Apple M4, 10 cores, darwin/arm64. `go test -benchtime 3000x -count 5`, **no `-race`**, no
  competing load. The gate instruments in this repository are load-sensitive in opposite directions,
  so a quiet machine is a precondition, not a nicety.
- Reported below: the median of five runs per arm.

## Result

| writers | commits/fsync | **fsyncs/commit** | ns/op | ops/sec |
|--:|--:|--:|--:|--:|
| 1 | 1.000 | 1.000 | 3 788 218 | 263 |
| 8 | 4.121 | 0.243 | 931 976 | 1 073 |
| 64 | 31.58 | 0.032 | 118 033 | 8 472 |
| 256 | 107.1 | 0.0093 | 35 051 | 28 530 |
| 1024 | 300.0 | 0.0033 | 12 712 | 78 667 |

Spread across the five runs is small at every arm — at 1024 writers `commits/fsync` ranged 272.7–375.0
and ops/sec 74 877–102 656, the widest of the ladder, which is expected where 1024 goroutines share 10
cores.

**fsyncs per commit falls 300×** from one writer to 1024, monotonically at every step, and throughput
rises **299×**. A single writer pays one whole fsync per commit by definition; at 1024 writers one
leader's fsync makes 300 committers durable.

## The number this table does not contain

This is the **store API** path. Its sibling arm, `BenchmarkWriteScaling_Cypher`, drives the same
durable single-edge write through the Cypher engine over the same WAL and the same in-memory graph,
and is **flat at ~268 commits/s at every writer count** — the figure the sprint audit recorded.

The difference is not the WAL. The Cypher path performs its fsync while the graph's visibility barrier
(`lpg`'s `visMu`) is held in write mode, so writers are strictly serialised across the disk sync and
can never coalesce; the store path releases the single-writer semaphore after the append and syncs
outside it, so many committers are inside `SyncGroup` at once.

Two paths over one WAL, one coalescing 300× and one unable to coalesce at all, localises the ceiling
to the barrier rather than to durability. Removing it is rmp #2304.

## Reproducing

```bash
go test ./store/txn/ -run '^$' \
  -bench '^BenchmarkWriteScaling_StoreAPI$' \
  -benchtime 3000x -count 5
```

Note the `^…$` anchors: `-bench` matches the name **without** the `Benchmark` prefix, so
`-bench BenchmarkWriteScaling_StoreAPI` silently matches nothing and the command exits 0 having run no
benchmark at all.
