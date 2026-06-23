# GoGraph multithread-optimization audit — 2026-06-22

Read-only, evidence-driven audit of the **whole module** (components individually and
working together) to identify where multithread (parallel) execution would most improve
performance. The examples are the **instrument**: realistic workloads used to exercise the
server and the analytics layer so the module can be measured empirically. Nothing in the
module was changed; the deliverable is this report plus a ranked `rmp` backlog and Knowledge
Graph entries.

All measurements were taken on a **10-core** machine, with the Go race detector **off**
(it perturbs scaling). Profiles and raw benchmark logs are retained in the session
scratchpad (`prof/`).

---

## 1. Method

Two workload dimensions were exercised, each mapped to the examples that embody it:

| Dimension | What it stresses | Exercised through |
|---|---|---|
| **Many concurrent clients** | reader/writer concurrency in the engine + Bolt server | `bench/soak` Bolt round-trip suite (same server stack as **ex.23** store-less Bolt and **ex.25** WAL-backed REST API) + **ex.20** concurrent reads; a throwaway engine-level harness to remove connection-churn noise |
| **One heavy analytics query** | intra-algorithm parallelism | `search/**` benchmarks for the algorithms used by **ex.03** advanced algorithms, **ex.08** PageRank, **ex.09** Leiden, **ex.16** centrality |

Instruments: `b.RunParallel` scaling sweeps (`GOMAXPROCS` 1→8), `pprof` CPU / mutex / block
profiles, and `-cpu=1,8` parallel-vs-sequential comparisons. Findings were validated by three
specialists (concurrency/perf, graph theory, Go idiom + ACID), grounded in the source — not memory.

---

## 2. Headline — which operations benefit most from multithreading

1. **Concurrent client reads, especially under a concurrent writer** (M1, M2). This is the
   biggest aggregate-throughput lever for the server, and the top item (M1) is a safe,
   near-trivial fix. Today reads **do not scale past 4 cores and regress below single-thread
   at 8**, and a single low-rate writer makes concurrent reads **3× slower**.
2. **Heavy analytics queries** (A1–A3, A8). The biggest single-query latency wins. The module
   already proves ~4.7× is reachable (parallel Brandes), yet the heaviest operators
   (Leiden ≈ 1.4 s, Johnson APSP) are fully sequential.
3. **Large scans / aggregations** (M3). A morsel-parallel scan operator already exists but is
   not wired into the planner, so every scan runs on one core.

Writes are **correctly** serialized by the single-writer visibility barrier (the cost of
atomicity); durability-side **group commit already exists** (task #1507). Concurrent writes to
disjoint regions are only unlocked by the larger M2 snapshot work — not a separate initiative.

---

## 3. Evidence

### 3.1 Scaling (engine-level, no Bolt — `RunParallel` ns/op; lower = more aggregate throughput)

| Workload | c=1 | c=2 | c=4 | c=8 | Verdict |
|---|---|---|---|---|---|
| Read `MATCH (n) RETURN count(n)` (2000 nodes) | 122 µs | 72 µs | 67 µs | **143 µs** | **does not scale — regresses past 4 cores** |
| Write autocommit `CREATE (:N)` | 5.10 µs | 5.59 µs | 5.61 µs | 5.64 µs | flat — fully serialized (expected) |
| Read with ONE concurrent explicit-tx writer (c=8 readers, 1212 writer txns) | — | — | — | **469 µs** | **≈3× slower than the 150 µs no-writer baseline** |

### 3.2 Read-path CPU profile @ c=8 (root cause of the cliff)

```
flat  flat%   symbol
31.8%         sync/atomic.(*Int32).Add        ← RWMutex reader-count, cache-line bounced across cores
28.7%         runtime.lock2                   ← RWMutex slow path (not GC: no mallocgc/scanobject near top)
24.5%         runtime.usleep                  ← lock spin/backoff
35.6% (cum)   lpg.(*Graph).IsTombstoned       ← of which tombstoneMu.RLock/RUnlock ≈ 99%; the map lookup is 0.9%
43.5% (cum)   lpg.(*Graph).View               ← visMu.RLock around the whole scan
```

`IsTombstoned` (`graph/lpg/lpg.go:1395`) takes a **global** `tombstoneMu.RLock()` **per node**;
the lock costs ~5.9 s of a 12.4 s sample, the actual `g.tombstones[id]` lookup ~0.1 s.

### 3.3 Read-under-writer block profile (mechanism)

```
27% cum  Engine.Run → Graph.View → sync.(*RWMutex).RLock      ← readers BLOCK here…
23% cum  Engine.BeginTx → LockBarrier → sync.(*RWMutex).Lock  ← …while a writer holds the exclusive barrier
```

### 3.4 Through the real Bolt server (mixed 80/20, c=8)

`sync/atomic.(*Int32).Add` is the **#2 CPU consumer at 14.4%**, behind only network syscalls
(30%) — the same RWMutex reader-count contention, confirmed end-to-end through the server stack
that ex.23 / ex.25 use.

### 3.5 Analytics (GOMAXPROCS 1 vs 8)

| Algorithm | cpu=1 | cpu=8 | Speedup | Status |
|---|---|---|---|---|
| Betweenness **Serial** | 7.89 ms | 7.98 ms | 1.0× | sequential |
| Betweenness **Parallel** (control) | 7.88 ms | **1.68 ms** | **4.68×** | parallel — proves the pattern |
| PageRank PowerLaw50K | 13.9 ms | 8.06 ms | 1.73× | parallel but sub-linear |
| **Leiden** (heaviest op) | 1409 ms | 1423 ms | **1.0×** | **sequential** |

Sequential by inspection (0 goroutine-spawn sites): `johnson.go`, `floyd_warshall.go`,
`wcc.go`, `triangles.go`, `community/leiden.go`, `centrality/brandes_weighted.go`,
`diameter.go`, `centrality/ppr_push.go`. Already parallel: `pagerank.go`, `brandes_parallel.go`.

---

## 4. Findings

### Dimension 1 — many concurrent clients

#### M1 — `IsTombstoned` per-node RWMutex is the dominant read cost & the multicore read-scaling killer  ·  **HIGH · surgical · SAFE**

- **Root cause:** every node scanned takes a global `tombstoneMu.RLock()`; under 8 cores all
  readers hammer one `readerCount` cache line (§3.2). 99% of `IsTombstoned`'s cost is the lock.
- **Fix:** the lock-free gate `tombstoneActive atomic.Int64` already exists (`lpg.go:325`) and is
  used by `AddNode` (`lpg.go:762`) but **not** by `IsTombstoned`. Add
  `if g.tombstoneActive.Load() == 0 { return false }` to `IsTombstoned`: on the common
  never-deleted graph this replaces a cache-line-bouncing `RWMutex.RLock` (an atomic
  read-modify-write) with a contention-free atomic load (a shared-state read, no coherence
  traffic). **No API change, ACID-safe, race-clean.**
- **Validation:** the concurrency specialist identified this exact safe fix; the Go/ACID
  specialist confirmed the broader "drop the inner lock unconditionally" idea is **unsafe**
  (see M1-note), so the gate is the correct surgical path.
- **Expected impact:** removes the read cliff; reads should scale toward NumCPU on no-delete graphs.

> **M1-note (why not just remove the inner lock under `View`):** the tempting "the scan already
> holds `visMu.RLock`, so skip the per-element lock" optimization is **unsound as a drop-in**.
> The premise "these maps are mutated only under `visMu.Lock`" is false: exported mutators
> (`SetNodeProperty`, `DelNodeProperty`, `RemoveNode`, `Revive`, `RestoreTombstones`) take only
> their inner lock and never `visMu`, and the read-only-tx path (#1573) runs with no `visMu` by
> design. Removing the inner lock would create a genuine `concurrent map read/write` race / torn
> `propBag`. Doing it safely requires gating every writer behind `visMu` (an API-contract change)
> — deferred to a separate decision; see M2.

#### M2 — the visibility barrier `visMu` (one global RWMutex) blocks readers behind writers  ·  **HIGH**

- **M2a (uncontended cost):** `View`'s own `visMu.RLock` reader-count atomic contributes to the
  c=8 read cliff even with no writer present (§3.2).
- **M2b (contended cost):** an explicit write transaction holds `visMu.Lock` **exclusive from
  BEGIN to COMMIT** — across every statement and the client network wait. One low-rate writer
  made concurrent reads ≈3× slower (§3.1, §3.3).
- **Fix ladder:**
  - **M2b-quick:** stop holding `visMu.Lock` across the explicit-tx client think-time; acquire it
    only for the COMMIT apply. Directly removes the 3× hit with no snapshot machinery. Requires an
    isolation re-audit (it narrows the window during which the barrier is held).
  - **M2-full:** copy-on-write `atomic.Pointer[Snapshot]` read path — **already specified** in
    `docs/isolation-design.md` (the deferred "F3" work). ACID-sound (validated): per-shard
    structural sharing, commit cost `O(shards touched)` independent of graph size, and it
    *strengthens* isolation (per-statement read-committed → per-statement snapshot isolation).
    Multi-sprint, highest risk — schedule last; reconcile the documented isolation contract when it lands.

#### M3 — morsel-parallel scan operator exists but is not wired into the planner  ·  **MEDIUM**

- `cypher/exec/parallel.go` implements `ParallelScan` (morsel/worker-pool, task-249) with tests,
  but it is **constructed only in tests** — the production planner emits the serial `AllNodesScan`.
  Every large scan/aggregation runs on one core.
- **Caveat:** its per-row output channel `outCh chan expr.Value` (cap `GOMAXPROCS*2`) is a funnel.
  Wiring it for aggregations (`count`, `sum`) should use a **parallel-reduce** (per-worker partial
  aggregate, combine at the end), not row-by-row channeling, or it will not scale.

#### M4 — heavy read-path allocation caps multicore scaling via GC  ·  **MEDIUM (cross-ref memory audits)**

- `MATCH (n) RETURN count(n)` over 2000 nodes allocates **125 KB / 3817 allocs** (~1.9 allocs/node);
  a bare `count(var)` still materializes per-row `NodeValue`s (a previously-deferred finding).
  At c=8 this is tens of millions of allocs/s → GC-assist becomes a serial drag.
- Cross-references the sprint 222/223 allocation work and the deferred "NodeValue construction for
  bare `count(var)`" item. Fewer allocations directly improve multicore scaling.

### Dimension 2 — one heavy analytics query (intra-algorithm parallelism)

Ranked by leverage (speedup × query cost × low risk). The proven template is
`search/centrality/brandes_parallel.go` (4.68× measured); the analytics path reads an **immutable
CSR snapshot**, so ACID is not at risk — only correctness/determinism.

| # | Task | Speedup (10c) | Risk | Determinism |
|---|---|---|---|---|
| **A1** | Parallel **weighted** Betweenness (copy the unweighted parallel sibling) | ~4.5–4.7× | very low | inherited (none) |
| **A2** | Parallel **Johnson APSP** (\|V\| independent SSSP; per-worker Dijkstra state) | ~6–8× | low | **bit-identical** |
| **A3** | Parallel **triangle counting** (per-vertex stripes, integer reduce) | ~5–8× | low–med | **bit-identical** |
| **A4** | PageRank dangling-sum → parallel partial | folds O(V)/iter serial term | low | ~1e-15 off → **DECISION** |
| **A5** | PageRank iteration barrier: channel rendezvous → counting barrier | ceiling ~2.5–3× | medium | none (benchstat-gated) |
| **A6** | WCC → Afforest concurrent union-find | ~4–6× | med–high | deterministic (min-id) |
| **A7** | Parallel Floyd-Warshall (inner loop per k) | ~4–6× (dense only) | med–high | **bit-identical** |
| **A8** | Leiden parallel local-moving (heaviest op) | ~4–5× | high | **breaks bit-identity → DECISION** |

**Do not parallelize:** `diameter` (sequential by nature), `ppr_push` (sublinear by design).

### Dimension 3 — writes

- Autocommit writes are correctly serialized by `visMu` — the inherent cost of transaction
  atomicity, not a defect. **Group commit already exists** (task #1507): the writer semaphore is
  released after the WAL append phase and fsync is coalesced via `wal.Writer.SyncGroup`. Concurrent
  application of disjoint writes is only unlocked by M2-full; there is no separate "group commit" gap.

---

## 5. Decisions required (per the project's decision-autonomy rule)

- **A4 — PageRank dangling-sum:** parallelizing changes the documented *bit-identical* result to
  *deterministic-for-fixed-worker-count* (≈1e-15 numeric drift), as already accepted for `delta` and
  `BetweennessParallel`. Approve?
- **A8 — Leiden:** parallel local-moving cannot preserve the bit-for-bit serial output by
  construction. Options: (a) leave serial [recommended now]; (b) add a separate `LeidenParallel`
  with an explicitly weaker, documented contract; (c) parallelize only the aggregation phase
  (~35% of runtime, deterministic).
- **M1-note / M2 API change:** making the under-`View` unlocked accessors safe requires routing
  every map mutator through the barrier (an API-contract change). Pursue, or stay with M1's safe gate?

---

## 6. Reproduction

```bash
# Bolt server scaling (real server stack — ex.23/ex.25), stderr is connection-churn noise:
go test -run='^$' -bench='BenchmarkBoltReadOnly/conc=(1|8)$'  -benchmem -benchtime=2s ./bench/soak/
go test -run='^$' -bench='BenchmarkBoltWriteOnly/conc=(1|8)$' -benchmem -benchtime=2s ./bench/soak/
go test -run='^$' -bench='BenchmarkBoltMixed/conc=8$' -cpuprofile=cpu.out -mutexprofile=mu.out ./bench/soak/

# Analytics parallel vs sequential:
go test -run='^$' -bench='BenchmarkBetweenness_(Serial|Parallel)$|BenchmarkPageRank_PowerLaw50K$|BenchmarkLeiden_RandomGraph$' \
  -benchtime=1s -cpu=1,8 ./search/...

# Engine-level read/write scaling + read-under-writer (clean, no connection churn):
# harness preserved at scratchpad/audit_bench_harness.go.txt — drop into a temp package and:
go test -run='^$' -bench='BenchmarkEngRead$|BenchmarkEngWriteAutocommit$|BenchmarkEngReadUnderWriter$' \
  -benchmem -benchtime=2s -cpuprofile=cpu.out -mutexprofile=mu.out -blockprofile=block.out .
```

Key source references: `graph/lpg/lpg.go` (`visMu` 358–415, `View` 492, `ApplyAtomically` 400,
`IsTombstoned` 1395, `tombstoneActive` 325/762), `graph/lpg/property.go:278`,
`cypher/exectx.go:221` (`BeginTx`/`LockBarrier`), `cypher/exec/parallel.go` (unwired `ParallelScan`),
`docs/isolation-design.md` (M2-full spec), `search/centrality/brandes_parallel.go` (parallel template).
