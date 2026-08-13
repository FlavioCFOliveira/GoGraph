# Tiering the per-pair edge-instance maps (rmp #2401)

**Sprint** 339 · **Date** 2026-08-11 · **Baseline** `b3887752`

Remediation of the largest finding of
[`../memory-vs-neo4j-memgraph-2026-08-11.md`](../memory-vs-neo4j-memgraph-2026-08-11.md): a
relationship created through Cypher cost **1 078.87 B**, of which 87.9 % was two nested per-edge
maps storing a relationship type the adjacency already holds in four bytes, and loading four
million relationships OOM-killed the engine in an 8 GB container.

## 1. The change

The four per-edge side stores were each a nested map:

```go
map[edgeKey]map[int64]labelBag    // per-CREATE ordinal
map[edgeKey]map[uint64]labelBag   // per stable handle
map[edgeKey]map[int64]propBag
map[edgeKey]map[uint64]propBag
```

The inner map is allocated **per node pair**, and a Go map holding one entry costs several
hundred bytes of header, control bytes and a whole eight-slot group. Almost every pair carries
exactly one relationship, so almost every one of those maps held exactly one entry.

`graph/lpg/instmap.go` introduces `instMap[K, V]`, the two-state union `propBag` (sprint 207,
#1587) and `labelBag` (sprint 221, #1629) already apply one level further in — a small unsorted
slice up to `smallInstMax = 8`, promoting one-way to a map beyond it — held **by value** in the
outer map. No public API, no on-disk format and no semantics changed.

**The key was deliberately not flattened.** A single `map[instanceKey]V` would cost less still,
but `RemoveEdge` drops a whole pair's instance state with one map delete and the MVCC pre-image
walks iterate exactly that pair's instances; against a flat map both become a scan of the whole
shard, turning an O(parallel edges) delete into an O(shard) one — and bulk delete is already this
engine's weakest path (#2400). The nesting is load-bearing for deletes; only the inner map's
representation was the defect.

## 2. Result

### 2.1 The fixture that OOM-killed the engine now completes

Same fixture, same container, same limits as the audit: 500 000 `:Person` nodes with an index and
4 000 000 `:KNOWS` relationships created through Cypher over Bolt, `--cpus=4 -m 8g`.

| | before | after |
|---|---|---|
| outcome | **OOM-killed at the 3.5-millionth relationship** | **completed, all 4 000 000** |
| resident at failure / completion | 8 371 MB (kernel `anon-rss`) | **2 457 MB** |
| marginal cost | **1 326.61 B/edge** (R² 0.9990, 7 points) | **483.92 B/edge** (R² 0.9609, 9 points) |
| | | **−63.5 %, a 2.74× reduction** |

The kernel log confirms the negative: the two `ggserver invoked oom-killer` records in this VM
are at uptimes 24 739 s and 25 307 s, both from the pre-fix audit runs; the post-fix run finished
at uptime 30 103 s having added none.

### 2.2 In-process, isolated from Bolt and the seek index

100 000 `:Person` nodes and 800 000 `:KNOWS` relationships created through Cypher, live heap after
a forced collection (`bench/memprobe`, `TestProbe_CypherEdges`):

| | B/edge |
|---|---:|
| before | **1 078.87** |
| after | **323.83** |
| | **−70.0 %, a 3.33× reduction** |

The in-process figure is lower than the container one because the container arm also carries the
Bolt session state, the `sid` index and the harness's own count queries. Both are reported because
both are real; the in-process one is what isolates the storage change.

Attributed by a heap profile taken while the graph is live, converted at 2²⁰ B per pprof MiB:

| Allocation site | before | after | change |
|---|---:|---:|---:|
| `setEdgeLabelAtInfo` (ordinal label store) | 490.1 | **128.4** | −73.8 % |
| `setEdgeLabelByHandleInfo` (handle label store) | 486.6 | **113.9** | −76.6 % |
| `IncEdgeCreateCount` | 37.6 | 30.3 | −19.4 % |
| `upsertEdgeLocked` (the adjacency) | 31.5 | 21.0 | — |
| `setEdgeLabelSlotsAtTx` (the `uint32` type column) | 13.8 | 17.0 | — |
| **the two label stores together** | **976.7** | **242.3** | **−75.2 %, 4.0×** |

The two stores remain the largest term — they are still two records of the same relationship type
— but in absolute terms they are now a quarter of what they were. Retiring one of them outright
is spike **#2403**, which requires a design decision that is the user's to take.

## 3. What was verified

- **Behaviour unchanged.** Full `make ci` **exit 0** (read from the log, not from the wrapper —
  the background-task notification reported 0 for a run that had exited 2), 122 packages, lint
  0 issues, aggregate coverage 87.1 %, TCK **3897 scenarios, 3897 passed, 0 failed, 0 undefined**.

  Two intermediate gate runs went red and neither was this change; both are recorded rather than
  re-run away. (1) The coverage stage failed with `_pkg_.a: no such file or directory` import
  errors — a build-cache artefact; the same stage passed standalone at the identical 87.1 %.
  (2) `TestGateCtx_ReturnsWithinItsBudget` in `graph/mvcc` failed a **5 ms latency budget at
  117.9 ms**, during a run started immediately after `go clean -cache` with the machine at a
  **load average of 17.3 on 10 cores**. `git diff --name-only main..HEAD -- 'graph/mvcc/*.go'`
  returns **zero files**, which is what proves the attribution, and the test passes **20/20** in
  isolation at load 3.2. A latency-budget assertion is a load question before it is a code
  question.
- **The read path is not the price.** Interleaved A/B over five rounds of
  `BenchmarkEdgeSideRead_*` (`git stash` between arms): **B/op and allocs/op byte-identical on
  every arm**, sec/op geomean **−2.33 %** — four of the five arms marginally faster, because a
  linear scan over a one-element slice beats a map probe, and one 3.25 % slower. At n=5 these are
  at the edge of significance; the defensible claim is that the read path is unchanged, which is
  what the byte-identical allocation counts establish independently of timing.
- **The new path is covered where nothing covered it before.** Every fixture in the tree creates
  one or two parallel edges, so promotion past `smallInstMax` happened nowhere. `instmap_test.go`
  pins the container (zero value, small-tier get/set/del, the promotion boundary, one-way
  promotion, early-exit iteration, and that `del` clears the vacated slot so a removed bag is not
  left reachable behind the slice length); `instmap_stores_test.go` drives **12 parallel edges**
  between one pair through all four stores' public API, removes one from the middle, and drops
  the whole pair.
- **The tests can fail.** Three mutations of `instMap` — not clearing the vacated slot, making
  promotion two-way, and ignoring the early-exit signal — are each caught by the unit tests. A
  fourth, making `get` never consult the promoted map tier, is caught by the *store-level* tests,
  which is what proves they genuinely reach the map tier through the public API rather than
  stopping at the small one.

## 4. What this does not fix

- The relationship type is still stored **three times** (adjacency column, ordinal store, handle
  store). That is #2403.
- `edgeHandleLabelShards` is populated when `Multigraph` is false. That was filed as #2402 on
  the suspicion of a defect; measurement showed the BEHAVIOUR is correct — the adjacency stamps a
  handle in either mode — and the **godoc** was wrong. Corrected there, and pinned by
  `TestHandleStores_PopulatedInBothStorageModes`.
- Node-side costs are untouched: a node with three properties still costs 505 B against
  Memgraph's 147 (#2404, #2405).
