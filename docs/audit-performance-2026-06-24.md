# GoGraph full performance audit — 2026-06-24

Read-only, evidence-driven audit of the **whole module** — every component, how they
interconnect, and the system as a whole — to find where CPU, memory, and storage can be
improved **without compromising the openCypher TCK (3897) or ACID** mandates, with particular
attention to behaviour under **extreme concurrency**. Nothing in the module was changed; the
deliverables are this report, a leverage-ranked `rmp` backlog (sprints **S-PA7…S-PA11**,
tasks **#1700–#1724**), and Knowledge-Graph entries.

This is the broadest pass to date. The four prior multithread audits (2026-06-22/23) closed the
analytics-parallelism frontier and the read-path *registry-lock* regression; this audit
**re-measured empirically** where the bottleneck moved after those fixes, and widened scope to the
**storage write path, recovery, Bolt, data structures, the Cypher engine internals, and GC**.

## Method

- **Machine:** 10 physical cores, Apple/arm64, Go 1.26.4. Race detector **off** for scaling runs
  (it perturbs scaling); allocation metrics (`allocs/op`, `B/op`) are contention-immune and were
  also gathered from specialist micro-benchmarks.
- **Instrumentation:** a throwaway engine-level scaling harness (`bench/mtaudit/`, retained) that
  bypasses Bolt to measure pure engine read/write scaling at `cpu=1,2,4,8,10` via `RunParallel`,
  plus CPU / mutex / block / heap `pprof` profiles on the regressing paths. Existing package
  benchmarks (storage, data structures, analytics) supplied breadth. Example 26 is the memory
  instrument.
- **All concurrency-scaling numbers were captured in isolation** (one workload at a time) so they
  are clean; the five mandated specialists then did code-level root-causing and design against
  those profiles (running only contention-immune allocation checks themselves).
- **Specialists consulted (in parallel):** `concurrency-architect`, `rust-perf-engineer`,
  `storage-engine-auditor`, `cypher-expert-consultant`, `graph-theory-expert`. Every finding is
  grounded in source `file:line` + a measured number, not memory.

## What already shipped since the prior pass (verified in code — not re-reported)

| Area | Shipped | Evidence |
|---|---|---|
| Registry exclusive-mutex → RWMutex+RLock | #1695/#1696 | `graph/lpg/property.go`, `lpg.go` |
| Pooled per-row `LazyNodeValue` | #1697 | `cypher/api.go` |
| Multigraph edge-instance prop/label RLock | #1698/#1699 | `graph/lpg/edge_instance_*.go` |
| `IsTombstoned` no-writer atomic gate | #1669 | `lpg.go` `tombstoneActive` |
| Per-row `ParallelScanProject` (no funnel) | #1682 | `cypher/exec/parallel_scan_project.go` |
| `ParallelCountScan` (>50k) | #1672 | `cypher/exec/parallel_aggregate.go` |
| Parallel Brandes/WCC/Floyd/triangles/PageRank | — | `search/**` |
| Group commit | #1507 | `store/wal`, `store/txn` |
| **Bounded LRU plan cache** | (extant) | `cypher/plan_cache.go`, cap 1024, DDL-invalidated |

> The plan cache is the key correction to a prior assumption: parse+plan is **already cached**;
> the ~1800 allocs/op of a trivial query are **per-row**, not parse/plan. The engine opportunity
> therefore moved entirely onto the per-row read path.

## Headline

1. **The per-row property read path is the surviving read-scaling wall**, and it has *cheap,
   ACID-safe* cures. Two contended reader-count atomics are taken **per row**: the interned-key
   registry `Lookup` (RWMutex) and the property-shard `RLock`. `sync/atomic.(*Int32).Add` is
   **18.9 %** of CPU on a small filter and **37.7 %** on a large parallel projection at c=10. Per-row
   property filters **regress past 4 cores** while the label path (resolved once, then index-scanned)
   scales cleanly to 10. Cures: COW `atomic.Pointer` registries (#1700) + an M1-style no-writer gate
   on the property shard (#1701).
2. **Intra-query parallelism oversubscribes under concurrent queries** — the single biggest
   *extreme-concurrency* gap. Every parallel operator grabs `GOMAXPROCS` workers with no cross-query
   coordination, so N concurrent queries × GOMAXPROCS workers thrash the scheduler; the large-graph
   parallel paths scale to 4c then **regress at 8–10c**. Cure: adaptive fan-out (#1705).
3. **Per-row allocation drives GC/madvise churn** — `runtime.madvise` is **29 %** of read CPU on the
   filter path; the 60k-row projection allocates **302k objects / 26 MB per query**, dominated by
   `ParallelScanProject.runWorker` (53.8 %) and scalar boxing. Cures: per-worker arena (#1702),
   de-boxing (#1704), GC tuning (#1706).
4. **Writes are correctly serialized** (autocommit 5.4 µs flat 1→10c) and **group commit is optimal**
   (concurrent commit amortizes fsync **106×**: 3.4 ms → 32 µs at 256 committers). The
   reader-under-writer 2.4–2.8× penalty remains the deferred `visMu` COW epic (#1671).

## Evidence

### Engine read/write scaling (`bench/mtaudit`, ns/op via RunParallel; lower = more throughput)

| Workload | c=1 | c=2 | c=4 | c=8 | c=10 | allocs/op | Verdict |
|---|---|---|---|---|---|---|---|
| `MATCH (n) RETURN count(n)` (2k) | 77µs | 41 | 27 | 28 | 24 | 1812 | scales to 4c, plateaus |
| `MATCH (n:N) RETURN count(n)` (2k) | 70µs | 36 | 18 | 17 | 16 | 1810 | **scales cleanly** (R2 ok) |
| `WHERE n.v>=0 RETURN count(n)` (2k) | 511µs | 283 | **150** | **227** | **257** | 3568 | **regresses past 4c** |
| `WHERE n.v>=0 RETURN n.v` (2k) | 1023µs | 547 | 307 | 436 | 519 | 7307 | regresses |
| count fast path (60k) | 530µs | 280 | 166 | 153 | 145 | 71 | scales, plateaus |
| filter+count (60k, ParallelCountScan) | 14.9ms | 8.5 | **4.4** | **6.9** | 5.8 | 119584 | **regress at 8–10c** |
| filter+project (60k, ParallelScanProject) | 38ms | 21 | **11** | **14.6** | 15 | 302856 | **regress at 8–10c** |
| `CREATE (:N)` autocommit | 5.4µs | 5.6 | 5.7 | 5.8 | 5.8 | 37 | **flat — correct serialization** |
| read @c=8 **+ 1 steady writer** | — | — | — | 57µs | 59 | — | **~2.4–2.8× vs no-writer** |

### Profile attribution (c=10)

- **Small filter** (`WHERE n.v>=0 RETURN count(n)`): CPU — `runtime.madvise` 29.4 %, `atomic.Int32.Add`
  18.9 % (RWMutex reader-count), `usleep` 17 %, `NodePropertyByID` 23 % cum. Mutex — `runtime.unlock`
  82 % (heap lock under alloc churn). Block — harness `WaitGroup.Wait` (not meaningful).
- **Large projection** (`RETURN n.v`, 60k): CPU — `atomic.Int32.Add` **37.7 %**, `NodePropertyByID`
  43.5 % cum, `RWMutex.RLock` 20.5 % cum, `madvise` 9.8 %. Heap — `ParallelScanProject.runWorker`
  53.8 %, `Result.materialize` 34.7 %, `lpgPropToExpr` boxing 4 %.

Root cause: `graph/lpg/property.go` `NodePropertyByID` takes the registry `Lookup` RLock (line ~433)
**and** the property-shard `s.mu.RLock()` (line ~438) **per row**; both shared reader-count cache
lines bounce across cores. Same signature as the shipped M1 `IsTombstoned` gate.

### Storage (single-threaded micro-benchmarks; allocs reliable)

| Benchmark | ns/op | allocs/op | B/op | Note |
|---|---|---|---|---|
| WAL `Append/fsync` | 3.6ms | 1 | 16 | fsync-bound (durability) ✅ |
| `txn CommitConcurrent` 1→256 goroutines | 3.4ms→**32µs** | 7–8 | ~600 | **group commit 106× amortization** ✅ |
| `snapshot WriteProperties` | 4.9ms | **84,070** | 16MB | ⚠️ checkpoint alloc hotspot (#1707) |
| `snapshot WriteLabels` | 675µs | 4053 | 1.9MB | ⚠️ same pattern (#1709) |
| `bulk Load_Large` Baseline | 274ms | **1.77M** | **1.21GB** | ⚠️ per-source COW growth (#1708) |
| `bulk Load_Large` Presize | 273ms | 1.77M | 1.21GB | ⚠️ **presize ineffective** (Mapper-only) |
| `bulk Load_Large` Parallel | 196ms | 1.77M | 1.25GB | only 1.4× — alloc-bound |
| `recovery Indexes /snapshot` | 20.8ms | **1.0M** | 31MB | ⚠️ `binary.Read` per field (#1710), one-time |
| `csrfile WriteToFile` / `ReadCSR` | 9.4ms / 18GB/s | 24 / 10 | — | ✅ zero-copy reads |

### Data structures (healthy) & analytics

- `NodeSet` (#1596): `ContainsSingleton` 1.06ns, 0-alloc. `index/hash Seek/Append` 16.6ns 0-alloc.
  `btree DeleteDistinct` 0-alloc; `RangeCountGate` 0-alloc. `adjlist HasEdge_HotCache` 54ns 0-alloc.
  Hub-relabel **Batch** path 67× better than naive PerSlot. CRC scratch/incremental 0-alloc.
- Minor: `btree LookupHot` 10 allocs/lookup (#1722, **latent** — not on the engine hot path);
  weighted Brandes uses jagged `[][]int` predecessors (#1715); generic union-find is 22× slower
  than the slice version but **confirmed not on any hot path** (cleared).
- Parallel Brandes 5.6×, weighted Brandes 6×, PageRank parallel — all working ✅.
  **Leiden** is the last sequential analytic: **1.43s, 96.5MB, 0 goroutines** (#1713/#1714).

## Findings → backlog (leverage-ranked)

### Tier 1 — read-path lock-free scaling · **S-PA7 (sprint 233)** · highest leverage, ACID/TCK-neutral

| # | Finding | Sev | Win |
|---|---|---|---|
| **1700** | COW `atomic.Pointer` registries (lock-free `Lookup`) | HIGH | kills 18.9–37.7 % reader-count atomic; pattern already in-tree for `Resolve` |
| **1701** | No-writer atomic gate on `NodePropertyByID` (M1-style) | HIGH | removes the per-row shard RLock; ACID-safe (View excludes writers); strict subset of #1671 |
| **1702** | `ParallelScanProject` per-worker row arena + presize `nodeIDs` | HIGH | kills the 53.8 % heap term → restores scaling past 4c |
| **1703** | Late-materialize bare `var.prop` projection (TCK temporal caveat) | HIGH | ~3–5 → ~1 alloc/row on the heaviest workload |
| **1704** | De-box `lpgPropToExpr` int64/float64 | MED-HIGH | ~60–180k allocs removed on a 60k numeric projection |

### Tier 2 — extreme-concurrency parallelism + GC · **S-PA8 (sprint 234)** · user-gated

| # | Finding | Sev | Win |
|---|---|---|---|
| **1705** | Adaptive intra-query fan-out (gate on inflight-query count) | **CRITICAL** | removes the 8–10c large-graph regression under concurrent load. **User decision:** self-tuning gate now vs global morsel pool later |
| **1706** | Opt-in `GOMEMLIMIT`/`GOGC` for read-heavy load | MED | cuts the 29 % madvise. **User decision:** opt-in config field vs docs-only (no silent global) |

### Tier 3 — storage write-path allocation · **S-PA9 (sprint 235)** · byte-format & ACID-neutral

`#1707` snapshot `WriteProperties` streaming (84k→hundreds) · `#1708` bulk CSR-direct counting-sort
build (1.2GB→tens; unblocks parallel >1.4×; **user decision** a vs b) · `#1709` `WriteLabels`
streaming (new `ForEach*LabelByID`) · `#1710` index `Deserialize` de-reflection (1M→tens, one-time) ·
`#1711` `metrics.Time` closure escape · `#1712` parallel checkpoint encode (**after #1707**).

### Tier 4 — analytics + algorithms · **S-PA10 (sprint 236)**

`#1713` Leiden 96.5MB reduction (bit-identical, no decision) · `#1714` **`LeidenParallel`** weaker
contract (**USER DECISION** — breaks bit-identity; ~8–12×@16c) · `#1715` weighted-Brandes flat arena
(port #1515) · `#1716` parallel diameter iFUB (bit-identical) · `#1717` `KShortestPathsLoopless`
resource budget (closes EXP-compute DoS) · `#1718` Stoer-Wagner heap MAO (O(V³)→O(VE+V²log V)).

### Tier 5 — opportunistic / low-priority · **S-PA11 (sprint 237)**

`#1719` memoize `hashJoinOrderSafe`+`InferParamTypes` onto plan-cache entry · `#1720` presize
`materialize` for full-scan plans · `#1721` skip 1-entry `RowContext` map for binding-free `EvalWith` ·
`#1722` btree `LookupAppend` (latent) · `#1723` parallel CREATE INDEX backfill · `#1724` adjlist
bulk `AddEdgesFrom` (only if bulk ingest becomes real).

## Clean — no action (verified)

- **Async correctness sweep: clean.** No goroutine leaks; every per-site spawn is bounded
  (`min(GOMAXPROCS, work)`, bulk capped at 16, bounded channels pre-filled); context cancellation
  honoured; `goleak` coverage in every spawning package. The one *systemic* gap is #1705
  (cross-query oversubscription), invisible to a per-site sweep.
- **Bolt server: exemplary** for extreme connection counts — hard 1024 connection cap enforced at
  accept (no goroutine spawned when full), per-connection deadlines (slowloris-safe at 30s), drained
  `Shutdown`, RESET/auth bypass fixed, typed backpressure error. A template for how #1705 should
  bound engine-internal parallelism system-wide.
- **Writes / group commit / WAL:** correct and optimal; no durability/ordering defect.
- **Most search algorithms** (Dijkstra/A*/Bellman-Ford/Tarjan/BCC/Hierholzer/Dinic/Push-Relabel/MCMF/
  Hopcroft-Karp/Prim/Kruskal) are pooled, iterative (no stack-overflow), and zero-alloc in steady
  state. CSR is textbook-optimal.

## Rejected (considered and dismissed with reason)

- **Parallel snapshot *encode* for a 3–4× win** — the encode is **fsync-serialized**; only the
  *compute/collection* phase is parallelizable, and only as a low-leverage checkpoint-window
  shortener **after** #1707 (filed as #1712, not the over-claimed version).
- **Parallel WAL replay** — records are order-dependent and recovery is a one-time startup cost.
- **Uncapping bulk `maxParallelism=16`** — the cap is *protective* against the very oversubscription
  in #1705.
- **Parallel multi-statement transactions** — inherently data-dependent; correctly sequential.

## Deferred (user decision already taken)

- **#1671 — `visMu` COW lock-free read snapshot** (reader-under-writer 2.4–2.8×). The only *sound*
  cure for concurrent reads under a steady writer; high risk (changes the isolation mechanism);
  parked in sprint 231. #1700/#1701 help the no-writer read path independently of it.

## Reproduction

```bash
# Engine read/write scaling + read-under-writer (harness at bench/mtaudit/, race OFF):
go test -run='^$' -bench='BenchmarkEngRead|BenchmarkEngWrite' -benchmem -benchtime=1s \
  -cpu=1,2,4,8,10 ./bench/mtaudit/

# Per-row filter regression root cause (CPU + mutex profiles @c=10):
go test -run='^$' -bench='BenchmarkEngReadFilterCount$' -benchtime=3s -cpu=10 \
  -cpuprofile=cpu.out -mutexprofile=mu.out ./bench/mtaudit/
go tool pprof -top cpu.out      # → atomic.Int32.Add (RWMutex reader-count) + madvise dominate

# Storage / DS / analytics breadth:
go test -run='^$' -bench=. -benchmem -benchtime=300ms ./store/{wal,snapshot,csrfile,recovery,bulk,txn}/
go test -run='^$' -bench=. -benchmem -benchtime=100ms ./search/centrality/ ./search/community/
```

Key source anchors: `graph/lpg/property.go:236,438` (per-row atomics), `cypher/exec/parallel_scan_project.go:157,259`
(arena + presize), `cypher/exec/parallel*.go` `runtime.GOMAXPROCS(0)` (oversubscription),
`cypher/api.go:7312` (`lpgPropToExpr` boxing), `store/snapshot/properties.go` (WriteProperties),
`store/bulk/bulk.go` (CSR-direct target), `search/community/leiden.go` (sequential), `cypher/plan_cache.go`
(extant plan cache).
