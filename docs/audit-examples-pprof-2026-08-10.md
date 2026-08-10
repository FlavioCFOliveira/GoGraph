# Improvement opportunities found by exercising every example under `pprof` — 2026-08-10

**Entry head:** `cbc45aa2` · sprint 337 · Apple M4 (10 cores, 32 GiB), `darwin/arm64`, go1.26.5

Every one of the 37 programs under `examples/` was driven under `-profile-dir`, and its CPU and heap
profiles read. This is the first sweep able to do that: the capability was only added to 35 of them
in the previous cycle (rmp #2377), and the reference-scale example had still never been profiled at
all.

Findings are ranked by the project's decision framework — **correct → secure → fast → efficient** —
not by size.

---

## Coverage

| | |
|---|---|
| Examples profiled | **37 / 37** |
| Runs producing a valid `cpu.pprof` + `heap.pprof` | **42** — example 24 contributes six, one per subcommand; every other example one |
| Examples with no valid profile | **0** |
| Examples needing non-default arguments | 24 (six subcommands), 25 (server lifetime), 26 (scale) |

Four further runs produced no profile and are each accounted for: examples 24 and 25 invoked with no
arguments exit 2 and print their usage, which is their documented behaviour; and example 26's two
over-budget attempts, below.

Example **24** was driven through `init → seed → stats → query → plandiff → snapshot`, each in its
own process. Example **25** was profiled across its whole server lifetime: startup recovery, a
synthetic seed, about 1 200 HTTP requests, and graceful shutdown on `SIGTERM`.

### Example 26 does not complete at its own default, and that is the headline

`26_social_scale_bench` is the project's reference end-state example. Its default is 1 000 000 users
and about 175 M edges.

| scale | outcome |
|---|---|
| default (1 M users) | killed at **12 min**; never printed past its config block |
| 150 000 users | killed at **7 min**, mid-query-battery — the build alone took **4 m 11 s** |
| 20 000 users | completed in **67 s**, first valid profile pair |

The 150 000-user run got far enough to print its own telemetry before being killed, and that
telemetry is the single most significant piece of evidence this sweep produced:

```
edges.friend=26249942   edges.like=22525717
# build.elapsed=4m11.083s   # build.edge_rate=194261 edges/s
# mem.heap_alloc=1.37 GiB   # mem.total_alloc=331.48 GiB   # mem.sys=47.31 GiB
# bytes_per_edge=30.2
# q.count_friend.latency=43.804822s
# q.count_like.latency=36.181658s
# q.friend_since_filled.latency=51.634874s
```

**331.48 GiB of total allocation to build a graph whose live heap is 1.37 GiB** — a 242× allocation
amplification — and the process asked the OS for **47.31 GiB**. A `count` over 26.2 M relationships
took **43.8 s**.

None of this is one defect, and none of it was visible to any gate, benchmark or test in the module,
because the workload that exhibits it has never run to completion under an instrument. Treat it as a
sprint, not a fix.

---

## 1. FIXED — dynamic chunks were never pre-sized (rmp #2381)

*Class: documentation accuracy (correct) + efficiency.*

`NewDynamicChunk`'s godoc opened with "each **pre-sized to capacity rows**" while its own next
paragraph stated "a dynamic column has **no backing at construction**". The two sentences
contradicted each other and the first was false: the constructor allocated nothing, and the `Put`
that committed a column to a type merely flipped the storage tag and appended into a nil slice. Every
fresh dynamic chunk therefore walked `append`'s entire growth series on its first fill.

Measured on one int64 column filled to the default 4096-row capacity, 3 rounds:

| arm | ns/op | B/op | allocs/op |
|---|---|---|---|
| dynamic, before | 16 990 | 128 248 | 16 |
| dynamic, **after** | **11 056** | **32 768** | **1** |
| static `NewChunk` (untouched control) | 12 810 → 12 762 | 32 960 → 32 960 | 3 → 3 |
| dynamic, `Reset` + refill (warm) | 9 885 | 0 | 0 |

The static control is byte-identical before and after, which is what rules out a machine-wide drift
reading. The warm arm stays at zero allocations, confirming the fix is free for pooled chunks.

**The fix** adds one `Chunk.commitDynamic` helper that sets the storage tag *and* allocates the
backing at the chunk's capacity when it has none, and routes all six commit paths through it
(`PutInt64`, `PutFloat64`, `PutString`, `PutBool`, `PutNull`, `PutValue`). `promoteToBoxed` had the
same defect one branch over — it built a slice of length `col.n` mid-fill — and is now sized to the
chunk capacity too. A `cap == 0` guard preserves `Reset`'s backing retention exactly.

### Measured where the finding was made — `examples/23_bolt_server`, interleaved, 3 rounds

The profile that surfaced it: `Chunk.pushI64` was **94.17 MB of the run's 213.42 MB (44.12 %)**,
reached 100 % through `tryBuildColumnarAggInput.evalPutColumnFiller → PutValue → PutInt64`, with
**72.70 %** of the run flowing through `EagerAggregation.consumeChunk`.

At `-nodes 20000 -queries 20000 -sessions 8`, arms alternating every round and never overlapping:

| metric | before | after | change |
|---|---|---|---|
| total allocation | 6.56 – 6.59 GB | **4.63 – 4.82 GB** | **−27.7 %** |
| throughput | 6 813 – 6 843 q/s | **7 401 – 7 474 q/s** | **+8.8 %** |
| latency p50 | 898 – 938 µs | 826 – 871 µs | −8.2 % |
| latency p99 | 3.127 – 3.204 ms | 2.960 – 2.993 ms | −6.1 % |

Every deterministic fact the example pins was **identical across all six runs**.

Pinned by `TestCommitDynamicPreSizesBacking` (all six commit paths),
`TestCommitDynamicFirstFillDoesNotAllocate` (12 allocations against 3, pre-fix),
`TestCommitDynamicReuseAllocatesNothing` (the reuse guard) and `TestPromoteToBoxedPreSizes`. All
four fail against the pre-fix build, verified in a scratch worktree at `cbc45aa2`.

---

## 2. OPEN — per-row property materialisation dominates the reference example

*Class: efficiency and speed. Largest measured opportunity. Needs a decision on scope.*

`examples/26_social_scale_bench` at 20 000 users allocates **33.48 GB** in 67 s. Half of it is the
per-row row-context build:

| site | flat | cum |
|---|---|---|
| `cypher.populateRowCtx` | 6 848.66 MB (20.46 %) | **16 595.58 MB (49.57 %)** |
| `cypher.buildRowCtxWithUse` | 973.04 MB | 9 872.21 MB (29.49 %) |
| `cypher.buildEdgeProps` | 2 140.52 MB | 6 226.84 MB (18.60 %) |
| `cypher.edgePropsToExprMap.func1` | 3 367.81 MB (10.06 %) | 3 608.82 MB |
| `cypher.nodePropsToExprMap.func1` | 2 283.55 MB (6.82 %) | 2 577.55 MB |
| `graph/lpg.(*edgePropCols).GrowSlotWithValue` | 2 368.32 MB (7.07 %) | 2 597.92 MB |

Its CPU profile — the first ever taken of this workload — agrees. String-keyed map work is the
largest identifiable cluster, and `madvise` is the GC handing the churn back to the OS:

| symbol | flat | cum |
|---|---|---|
| `runtime.mapaccess2_faststr` | 2.85 % | **12.90 %** |
| `internal/runtime/maps.getWithoutKeySmallFastStr` | 5.42 % | 8.75 % |
| `runtime.madvise` | 6.06 % | 6.06 % |
| `internal/runtime/maps.(*Iter).Next` | 3.49 % | 4.05 % |
| `aeshashbody` | 3.47 % | 3.47 % |
| `graph.fnv1aString` | 2.25 % | 2.25 % |

The module already has the machinery that should prevent this: the lazy/partial node materialisation
of #1500/#1659, gated by `analyseNodeScalarUse`. The open question is **why the gate is not engaging
for this example's query shapes** — whether the queries genuinely need whole entities, or whether the
analysis bails. That is the first thing to measure, before any redesign.

---

## 3. OPEN — the physical plan is rebuilt on every execution

*Class: efficiency. Two parts are in scope today; the third is an architecture decision.*

`examples/35_mvcc_mixed_workload` allocates 9 125.86 MB, and **`cypher.(*Engine).buildReadPhysical`
is 5 959.06 MB of it — 65.30 % of the run's entire allocation.**

The *logical* plan is already cached: `planCache` is a bounded LRU keyed by query text, and its entry
already memoises several pure functions of the immutable plan (`paramTypes`, `reorderCandidates`,
`pushedSeekHints`). The *physical* build is per execution by design, because it binds to the live
snapshot.

Two of its largest costs are, however, pure functions of inputs that do not change between executions
of the same cached plan, and are therefore the same class of thing the entry already memoises:

| candidate | cost in example 35 | why it is a candidate |
|---|---|---|
| `analyseNodeScalarUse` | **16.62 %** cum (1 516.73 MB) | a pure function of an immutable AST expression. Every write to a `nodeScalarUse` in the package happens inside the analysis itself — verified by enumerating all field writes — so the result is already treated as immutable by every consumer. |
| `copySchema` | **11.22 %** (1 024.17 MB) | a defensive shallow map copy, taken at 33 call sites per build. |

Memoising `analyseNodeScalarUse` on the plan-cache entry is precedent-backed and result-identical.
Caching the **physical plan** itself would be far larger and is an architecture change — it needs the
maintainer's decision before any work starts.

---

## 4. OPEN — example 20 demonstrates the costly PageRank API

*Class: efficiency, and the quality of what the example teaches.*

`examples/20_concurrent_reads` calls the one-shot `centrality.PageRank` inside its per-worker read
loop. The one-shot rebuilds the reverse-CSR transpose on every call, so:

| site | share of example 20's 204.60 MB |
|---|---|
| `search/centrality.pageRankBuildReverseStructure` | **74.32 MB — 36.32 %** |
| `search/centrality.PageRankCtx` (cum) | 95.37 MB — 46.61 % |

The module already ships the reuse path and documents it: `PageRanker` caches the transpose across
runs, and its godoc explicitly says to "give each goroutine its own PageRanker … because the
underlying CSR is immutable and read-only, independent PageRankers over the same snapshot are
race-free" — exactly this example's shape. This is an example-level fix that also corrects what the
example demonstrates.

---

## 5. INFO — default scale defeats CPU attribution

34 of the 37 examples finish in ≤ 2 s at their deterministic default. At 100 Hz that is roughly a
hundred CPU samples, and the aggregate CPU across the whole default sweep put no GoGraph symbol above
**0.11 s**. The heap profiles are informative at default scale because allocation counting does not
depend on run length; the CPU profiles are not. Any future cycle reading CPU from an example must
raise its scale first — as this one did for 26.

---

## On the security rung of correct → secure → fast → efficient

This sweep produced **no security finding, and it is not evidence of their absence.** A CPU or heap
profile of a benign, seeded workload is not a security instrument: it shows where a cooperative
program spends its time, never what a hostile input could make it do. The one class it could in
principle surface — an allocation whose size is a function of untrusted input with no ceiling —
cannot appear here, because no example feeds itself hostile input. Security assurance for these paths
rests on the dedicated audits (`docs/audit-production-readiness-*`, `docs/security-*`) and on the
existing input ceilings such as the 128 MiB interchange cap, not on anything measured below.

## Checked and clean

- **Retention.** The heap profile is written after `runtime.GC()`, so its `inuse_space` is live heap
  at exit. Across every default-scale run it was **0.5 – 4.0 MB**. No workload retains memory it has
  finished with.
- **All 43 runs exited 0** (after the two examples needing arguments were given them).

## Checked, not concluded

- **`buildEdgeTypeFilter` is 1 872.70 MB (5.59 %) of example 26**, reached entirely through the
  cache's miss path (`edgeTypeFilterFor.func1`). The cache is Engine-shared and keyed on the
  topology epoch, and the example performs writes between its query groups, which would be a
  sufficient explanation — but this was **not confirmed** against the cache's own hit/miss counters
  and should not be recorded as "working as intended" until it is.

## A harness lesson worth keeping

The first sweep bounded each example with `perl -e 'alarm shift; exec @ARGV'`. **That does nothing to
a Go program**: the Go runtime marks `SIGALRM` as `_SigNotify`, so with no goroutine listening the
signal is ignored rather than fatal. Example 26 ran unbounded for 12 minutes under a timeout that had
never been able to fire. Every later run used an explicit `SIGTERM`-then-`SIGKILL` watchdog, and the
watchdog was confirmed to fire.
