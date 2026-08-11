# The delete defect (rmp #2400, #2418): attribution and the in-edge index

**Date:** 2026-08-11 · **Branch:** `sprint-342` · Apple M4 (10 cores), macOS, `go1.26.5` ·
All figures produced in this cycle, in process — no container, no Bolt, no VM.

This document does two things. It **corrects the root cause** that
`docs/concurrency-vs-neo4j-memgraph-2026-08-11.md` §6 recorded for finding F1, and it
records the before/after evidence for the fix that replaced it, including what that fix
costs on the write path.

---

## 1. The correction

The assessment stated, under "Root cause, confirmed at source":

> `graph/lpg/lpg.go:2888-2899` (`removeNodeInfo`) takes the global `tombstoneMu` and calls
> `cur.Clone()` — a deep copy of the entire roaring64 tombstone bitmap — **once per node
> removed**.

That reading of the code is accurate, and it is **not what made the delete slow.** It was
never measured; it was inferred from reading the code, and "confirmed at source" meant
confirmed to *exist*, not confirmed to *cost*.

Two measurements refute it.

**A microbenchmark of the named mechanism.** `BenchmarkBulkNodeDelete`
(`graph/lpg/tombstone_bulk_delete_bench_test.go`) removes a fixed batch of 2 000 nodes from
graphs carrying different numbers of pre-existing tombstones. If the clone were the cost,
per-node time would rise linearly with the accumulated set:

| pre-existing tombstones | 0 | 20 000 | 40 000 | 80 000 | 160 000 |
|---|---:|---:|---:|---:|---:|
| ns per node removed | 831 | 1 118 | 1 172 | 1 211 | 1 273 |

A factor of 1.53 across an eightyfold increase in the set being cloned — because a roaring
bitmap holding a dense id range compresses to a handful of containers, so cloning it is
nearly independent of cardinality. Nothing here can produce the 4.8× the assessment saw.

**A CPU profile of the real path.** Reproducing the assessment's seed-and-wipe cycle in
process (`TestZZDeleteCycleRepro`, since promoted to the gates in §4) and profiling it:

| symbol | cum |
|---|---:|
| `exec.(*DeleteNode).Next` | 83.65% |
| `graph.(*Mapper[…]).Walk` | **78.77%** |
| `cypher.(*lpgMutatorAdapter).InNeighbours.func1` | 43.72% |
| `adjlist.loadEntry` | 9.76% |
| `lpg.(*Graph[…]).removeNodeInfo` | 1.72% |
| `roaring64.(*Bitmap).Clone` | **0.99%** |

Total samples 11.07 s of a 12.32 s run.

**The actual root cause.** `lpgMutatorAdapter.InNeighbours` answered "which nodes hold an
edge into n" by walking **every interned node** and scanning that node's whole adjacency
entry. The delete path asks it once per node deleted — `cypher/exec/delete.go:321` for the
plain-DELETE "does this node still have relationships" guard, and
`cypher/exec/detach_delete.go:177` and `:255` to remove the incoming edges — so deleting
*k* nodes from a graph of *n* cost O(k·n).

That explains every symptom the assessment recorded and could not attribute:

- **growth across cycles** — a deleted node keeps its Mapper slot for ever (NodeID
  stability is a hard contract), so *n* counts every node ever interned, not the live ones;
- **flat cost within one wipe** — *n* does not change during a wipe;
- **exactly one core** — the walk is serial;
- **`count` staying at 2–3 ms** — counting goes through the label index and never walks.

## 2. The defect, reproduced in process

Six seed-and-wipe cycles against ONE live engine, identical work each cycle.

| cycle | `DELETE` 20 000 nodes | `DETACH DELETE` 5 000 nodes + 2 500 rels |
|---:|---:|---:|
| 1 | 990 ms | 122 ms |
| 2 | 1.771 s | 178 ms |
| 3 | 2.349 s | 256 ms |
| 4 | 3.061 s | 359 ms |
| 5 | 3.841 s | 540 ms |
| 6 | 4.622 s | 616 ms |
| **last / first** | **4.67×** | **5.04×** |

The relationship path degrades exactly as the node path does, which **answers the question
§8 of the assessment left open** ("whether F1 has an equivalent on the edge-delete path —
only node delete was measured"): it does, and it has the same single cause.

Reachable as a failure, not merely as slowness: deleting 90 000 nodes in one statement took
**15.97 s** against `bolt/server`'s `DefaultTxTimeout` of 30 s — the margin the assessment
watched a benchmark fixture fall through twice.

## 3. The fix

`graph/adjlist/reverse.go` adds a live in-edge index to the adjacency: for every node, the
nodes holding an edge into it, maintained by the same funnels that publish an edge slot
(`upsertEdgeLocked` for insertion, the four removal paths for removal). `InNeighbours`
becomes O(in-degree). Neo4j and Memgraph both store the incoming direction beside the
outgoing one; this is the same decision, carrying NodeIDs only.

It is sharded on the destination's own shard bits and is a **leaf in the lock order** — it
never reaches back for an adjacency lock, so the order adjacency→reverse cannot invert.
Rollback needs no special handling because the undo log replays ordinary add and remove
operations rather than restoring entry snapshots.

| cycle | `DELETE` before → after | `DETACH DELETE` before → after |
|---:|---:|---:|
| 1 | 990 ms → **97.6 ms** | 122 ms → **27.0 ms** |
| 6 | 4.622 s → **77.2 ms** | 616 ms → **27.8 ms** |
| last / first | 4.67× → **0.79×** | 5.04× → **1.03×** |

**59.9× at the sixth cycle** for `DELETE`, **22.2×** for `DETACH DELETE`, and the growth is
gone: the curve is flat, which is the property that matters, because the multiple rises
without bound as a graph ages.

Single-statement delete of 90 000 nodes: **15.97 s → 375.6 ms (42.5×)**.

## 4. Gates

`cypher/delete_scaling_test.go` — all three fail on the pre-fix build and pass on the fixed
one, verified by building both arms from the same test file:

| gate | layer | pre-fix | fixed | threshold |
|---|---|---:|---:|---|
| `TestDeleteDoesNotDegradeAcrossCycles` | short | 4.67× | 0.79× | 2.5× |
| `TestDetachDeleteDoesNotDegradeAcrossCycles` | short | 5.04× | 1.03× | 2.5× |
| `TestSingleStatementDeleteOfNinetyThousandNodes` | soak | 15.97 s | 375.6 ms | 10 s |

The threshold is set **between the two measured regimes** rather than asserting a direction,
which noise alone would trip.

**Why the third gate is soak and the first two are not.** It asserts an absolute wall-clock
budget, and the short layer runs under `go test -race` with the package's other parallel
tests competing for the same cores. There it took **40.61 s** for work that takes 375.6 ms
on a quiet machine, and failed `make ci` for that reason rather than for a regression. The
first two are self-normalising — contention inflates the first cycle and the last alike —
and they hold their shape under race, measuring 1.00× and 0.99× where the pre-fix build
gives 4.67× and 5.04×. The regression property stays in the gate that runs on every change;
the absolute timing moved to the layer where timing means something.

`graph/adjlist/reverse_test.go` checks the index against the full scan it replaced — the
old implementation is kept there as the oracle — over randomised add/remove sequences in
all four graph configurations, plus self-loop exclusion and parallel-edge multiplicity. Its
ability to fail was verified by deleting one of the four `rev.remove` call sites, which
makes it report a stale source.

## 5. What the fix costs

Interleaved A/B, six alternating rounds of separately compiled before/after binaries,
`benchstat` n=6.

**Adjacency write path, in isolation** — the worst case, because this benchmark does
nothing but touch the adjacency:

| | sec/op | B/op |
|---|---:|---:|
| geomean over 13 write benchmarks | **+15.46%** | **+16.27%** |

**End-to-end relationship creation through Cypher** — the number that matters, because a
real write does far more per edge than touch the adjacency:

| | before | after | delta |
|---|---:|---:|---|
| `BenchmarkCreateRelationships` sec/rel | 4.029 µs | 4.106 µs | **+1.92%** (p=0.004) |
| B/op | 2.861 MiB | 2.920 MiB | +2.03% (p=0.002) |
| allocs/op | 17.07 k | 17.57 k | +2.93% (p=0.002) |

**Read path** — unchanged, as it must be, since no read path was touched:

| | before | after | delta |
|---|---:|---:|---|
| `BenchmarkAdjList_HasEdge_HotCache` | 56.62 ns | 55.63 ns | ~ (p=0.310, n=5) |

allocations 0 in both arms.

### 5.1 The first implementation was 10× worse than no index at all

The first draft grew the per-shard array to exactly `intra+1` on every new destination,
reallocating and copying the whole array each time — O(n²) where every edge lands on a
fresh destination. Measured against the no-index baseline on `BenchmarkHub_AddEdge_100k`:

| | sec/op | B/op |
|---|---:|---:|
| exact growth (first draft) | +296.44% | **+1056.87%** (45.48 MiB → 526.10 MiB) |
| geometric growth (kept) | +19.42% | +16.06% |

Recorded because the defect was invisible to every correctness test — the index was
correct throughout — and only the benchstat comparison the user required before accepting
the change exposed it.

### 5.2 A shared counter was removed before it shipped

The draft also kept its own `atomic.Int64` of recorded in-edges, incremented on every
insertion, purely to gate the empty-graph case. That is a second globally shared cache line
on the write path for a number `AdjList.Size` already maintains — the exact shape
`graph/mvcc/horizon.go` exists to avoid. The gate now reads `Size`, and
`RecordedInEdges` counts on demand for tests.

## 6. Reproduce

```bash
# The gates (fast on the fixed build; the first two fail on the pre-fix build).
go test -run 'TestDeleteDoesNotDegrade|TestDetachDeleteDoesNotDegrade|TestSingleStatementDelete' -v ./cypher/

# The index against the full-scan oracle, under the race detector.
go test -race -run TestReverseIndex ./graph/adjlist/

# The correction in §1: the named root cause, priced.
go test -run='^$' -bench=BenchmarkBulkNodeDelete -benchtime=3x -benchmem ./graph/lpg/

# The cost in §5, both arms built from the same tree with the implementation
# stashed for the "before" binary:
#   git stash push -u -- graph/adjlist/adjlist.go graph/adjlist/reverse.go cypher/api.go
#   go test -c -o /tmp/cy_before.test ./cypher/ && git stash pop
go test -c -o /tmp/cy_after.test ./cypher/
for i in 1 2 3 4 5 6; do
  /tmp/cy_before.test -test.run='^$' -test.bench=BenchmarkCreateRelationships \
    -test.benchmem -test.benchtime=300ms >> /tmp/cr_b.txt 2>/dev/null
  /tmp/cy_after.test  -test.run='^$' -test.bench=BenchmarkCreateRelationships \
    -test.benchmem -test.benchtime=300ms >> /tmp/cr_a.txt 2>/dev/null
done
benchstat /tmp/cr_b.txt /tmp/cr_a.txt
```

Send the engine's stderr somewhere other than the benchmark's stdout, as above: the
non-multigraph WARN interleaves into the benchmark result line and `benchstat` then drops
every sample with "parsing iteration count: invalid syntax".
