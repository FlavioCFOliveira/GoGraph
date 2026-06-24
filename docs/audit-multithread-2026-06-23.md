# GoGraph multithread-optimization audit — 2026-06-23

Read-only, evidence-driven audit of the **whole module** to find where multithread (parallel)
execution would most improve performance **without compromising correctness**. The examples are
the **instrument**: realistic workloads that exercise the same engine, server, and analytics
stack so the module can be measured empirically. Nothing in the module was changed; the
deliverable is this report plus a leverage-ranked `rmp` backlog and Knowledge Graph entries.

This is the **fourth** concurrency pass. It builds on `docs/audit-multithread-2026-06-22.md`
(which produced sprints 225/226). Its purpose is to (1) confirm what shipped, (2) find where the
bottleneck **moved** now that the prior top item (M1) is fixed, and (3) surface **new**
opportunities the prior pass could not see.

All measurements were taken on a **10-core** machine with the Go race detector **off** (it
perturbs scaling). Profiles and the throwaway engine-level harness are retained in the session
scratchpad (`prof/`, `audit_bench_harness_2026-06-23.go.txt`). Findings were validated by the
`concurrency-architect` specialist, grounded in the source and in profiles — not memory.

---

## 1. What already shipped since the prior audit (verified in code)

| Prior finding | Status | Evidence in code |
|---|---|---|
| **M1** `IsTombstoned` per-node lock | **FIXED** (#1669) | `tombstoneActive.Load()==0` gate at `graph/lpg/lpg.go:1404` |
| **M3-partial** parallel count scan | **SHIPPED** (#1672) | `ParallelCountScan` default-on >50k, `cypher/exec/parallel_aggregate.go` + `cypher/api.go` |
| **A1–A3, A6, A7** analytics | **PARALLELIZED** | `search/{wcc,johnson,floyd_warshall,triangles}_parallel.go`, `search/centrality/brandes_{,weighted_}parallel.go`, `pagerank.go` |
| **A8** Leiden | **still sequential** (deferred by user) | `search/community/leiden.go` — 0 goroutine sites |
| **M2 / #1671** visMu COW snapshot | **deferred** (multi-sprint ACID epic) | spec in `docs/isolation-design.md` (F3) |
| **#1682** full per-row `ParallelScan` | **still unwired** | `cypher/exec/parallel.go` constructed only in tests |

The analytics frontier is essentially closed: of the operators flagged last pass, **only Leiden
remains sequential**, and that is a standing user decision (parallel local-moving breaks
bit-identity). The remaining opportunities are all on the **engine read path** and the
**isolation barrier**.

---

## 2. Headline — where multithreading helps most now

1. **Concurrent client reads that touch a property or label predicate** (R1/R2 — **NEW**, safe,
   surgical). This is the single highest leverage × safety item remaining. The interned-key
   registries serialize *every* such read on one **exclusive** mutex; the common query shape
   (any `WHERE n.prop …` or `:Label` match) does not scale and **regresses past 4 cores**.
2. **Concurrent reads under a concurrent writer** (M2 / #1671). Still the deferred epic: a single
   steady writer makes reads **~2.6× slower**; the COW snapshot is the only sound cure.
3. **Large per-row scans / aggregations that are not the bare-count fast path** (#1682). The
   filter/projection scan runs on **one core per query** and the funnel design regresses under
   concurrency; needs a parallel-reduce redesign.

Writes remain **correctly** serialized by the single-writer visibility barrier (~5.4 µs flat,
1→8 cores) — the inherent cost of atomicity, not a defect. Group commit already exists (#1507).

---

## 3. Evidence (engine-level harness, `RunParallel` ns/op, lower = more throughput)

| Workload | c=1 | c=2 | c=4 | c=8 | allocs/op | Verdict |
|---|---|---|---|---|---|---|
| `MATCH (n) RETURN count(n)` (count fast path) | 77 µs | 41 | 28 | 29 | 1812 | scales to 4c, plateaus at 8 (**M1 fixed the old regression**) |
| `MATCH (n) WHERE n.v >= 0 RETURN count(n)` (per-row path) | 569 µs | 305 | 181 | **301** | 5567 | **REGRESSES past 4 cores** |
| `CREATE (:N)` (autocommit write) | 5.4 µs | 5.6 | 5.7 | 5.6 | 37 | flat — fully serialized (expected/correct) |
| read @c=8, no writer | — | — | — | 29 µs | — | baseline |
| read @c=8, **one steady explicit-tx writer** | — | — | — | **76 µs** | — | **~2.6× slower** (M2b still present) |

### 3.1 Filter-path regression root cause (CPU + mutex profiles @c=8)

```
CPU:    runtime.usleep                       48.4%   ← lock spin/backoff
        runtime.pthread_cond_wait            14.9%   ← blocked on lock
        internal/sync.(*Mutex).Lock/Unlock  ~14%     ← a PLAIN exclusive Mutex
        lpg.(*Graph).NodePropertyByID        29.0% cum
MUTEX:  sync.(*Mutex).Unlock                 89.4%  of all mutex delay
        path: Engine.Run → materialize → buildOperator → … → NodePropertyByID
```

The `concurrency-architect` reproduced this three independent ways: an isolated micro-bench where
the exclusive `Lookup` **regresses 10×** from 1→8 cores while the lock-free sibling `Resolve`
*improves 5×*; a `pprof -peek` attributing **100% of `sync.(*Mutex).Unlock` delay to
`PropertyKeyRegistry.Lookup`**; and the engine filter-path regression above.

---

## 4. Findings

### R1 / R2 — interned-key registries use an **exclusive** mutex on a **read-only** lookup · **HIGH · surgical · SAFE · NEW**

- **Root cause:** `PropertyKeyRegistry.Lookup` (`graph/lpg/property.go:231`) and
  `LabelRegistry.Lookup` (`graph/lpg/lpg.go:117`) both do `r.mu.Lock(); defer r.mu.Unlock()`
  around a pure `id, ok := r.forward[name]` map read. `Lookup` is hit **once per property access
  and once per label check, per row, per goroutine**. Under N readers all of them serialize on
  one exclusive lock — strictly worse than an RWMutex. The node-property *shard* already uses an
  `RWMutex` correctly (`property.go` `s.mu.RLock()`); the registries are the surviving funnel.
- **Why the prior audit missed it:** it benchmarked only bare `count(n)`, which touches no
  property or label, so the registry path was never on the hot path. This is the structural
  analogue of M1, but for the **common** query shape rather than the bare count — hence higher
  aggregate-throughput leverage than anything else remaining.
- **Fix (recommended): `sync.RWMutex` + `RLock()` on the read-only methods** (`Lookup` and any
  sibling read accessors), keeping `Lock()` only on intern of a brand-new key (rare). Validated as
  race-clean by construction, **no ACID / determinism / API impact**, and lock-ordering safe (the
  registry lock is released before any shard lock is taken — no ordering hazard). **No user
  decision required.** A copy-on-write `atomic.Pointer[map]` would give fully lock-free reads but
  is unnecessary complexity here; RWMutex is the right surgical fix.
- **Caveat (specialist):** the `:Label` path *plateaus* rather than regresses; the **property**
  path regression is registry-lock contention **plus M4 allocations acting together**. The lock
  fix alone will not bring the property path fully to NumCPU — **bundle R1/R2 with M4**.

### R3 / R4 — per-instance multigraph edge prop/label read locks · **MEDIUM · SAFE**

- Same class as R1/R2, narrower blast radius (multigraph parallel-edge instances only):
  read paths take an exclusive/sharded lock where an `RLock` suffices —
  `graph/lpg/edge_instance_props.go:99` and `graph/lpg/edge_instance_labels.go:107`.
- Lower priority than R1/R2 (only fires on multigraph instance-property/label reads), but the same
  cheap RWMutex treatment applies and is worth bundling.

### Clean (no action) — confirmed correct during the breadth sweep

- `graph/mapper.go:299` `Mapper.Lookup` — already `RWMutex.RLock()`; this is exactly why the
  registry is the survivor on the hot path.

### M2 / #1671 — visibility barrier `visMu` blocks readers behind writers · **HIGH leverage · multi-sprint · deferred**

- One steady explicit-tx writer makes concurrent reads **~2.6× slower** (§3); the reader-count
  atomic on `visMu.RLock` also contributes to the no-writer plateau at 8 cores.
- The only **sound** cure is the COW `atomic.Pointer[Snapshot]` read path already specified in
  `docs/isolation-design.md` (F3). Highest risk; needs the crash-injection battery + soak
  verification; **user previously deferred it as a dedicated epic.** Unchanged recommendation:
  schedule last, after the cheap registry/scan wins.

### #1682 — full per-row `ParallelScan` is unwired; filter scan runs on one core · **MEDIUM**

- The filter/projection path (anything that is not the bare-count fast path) executes serially per
  query *and* regresses under concurrency. `cypher/exec/parallel.go`'s per-row `outCh` funnel
  regressed ~2× when wired, so it needs a **parallel-reduce** redesign (per-worker partials,
  combine at end) like `ParallelCountScan` — not row-by-row channeling.

### M4 — read-path allocation caps multicore scaling via GC · **MEDIUM · bundle with R1/R2**

- Count path 1812 allocs/op; filter path **5567 allocs/op**. At c=8 this is tens of millions of
  allocs/s → GC-assist becomes a serial drag and is the *co-cause* (with the registry lock) of the
  property-path regression. Fewer allocations directly improve multicore read scaling.

### A8 Leiden — only remaining sequential analytic · **deferred (user decision)**

- `search/community/leiden.go` is the heaviest analytic and fully sequential. Parallel
  local-moving cannot preserve bit-identical serial output by construction. Options unchanged:
  (a) leave serial; (b) separate `LeidenParallel` with a documented weaker contract; (c)
  parallelize only the deterministic aggregation phase. Needs a user decision before any work.

---

## 5. Leverage-ranked backlog (post-M1)

| # | Item | Leverage | Risk | Decision needed | Notes |
|---|---|---|---|---|---|
| 1 | **R1/R2** registry `Lookup` → RWMutex+RLock | **HIGH** | very low | no | common query shape; bundle with M4 |
| 2 | **M4** read-path alloc reduction | MED–HIGH | low | no | co-cause of property-path regression; bundle with #1 |
| 3 | **R3/R4** multigraph instance read locks → RLock | MED | very low | no | same class, narrower |
| 4 | **#1682** parallel-reduce per-row scan | MED | medium | no | redesign funnel; mirrors ParallelCountScan |
| 5 | **M2/#1671** visMu COW snapshot | HIGH | high (multi-sprint) | yes (epic) | only sound cure for reader-under-writer 2.6× |
| 6 | **A8** Leiden parallel | MED | high | **yes** | breaks bit-identity |

**Recommended sequencing:** do 1+2 together first (cheap, safe, biggest common-case win), then 3,
then 4; treat 5 and 6 as user-gated.

---

## 6. Reproduction

```bash
# Engine-level read/write scaling + read-under-writer (clean, no Bolt connection churn).
# Harness preserved at scratchpad/audit_bench_harness_2026-06-23.go.txt — drop into bench/mtaudit/ and:
go test -run='^$' -bench='BenchmarkEngRead$|BenchmarkEngReadFilter$|BenchmarkEngWriteAutocommit$' \
  -benchmem -benchtime=1s -cpu=1,2,4,8 ./bench/mtaudit/
go test -run='^$' -bench='BenchmarkEngReadUnderWriter$' -benchmem -benchtime=2s -cpu=8 ./bench/mtaudit/

# Filter-path regression root cause (CPU + mutex profiles):
go test -run='^$' -bench='BenchmarkEngReadFilter$' -benchtime=3s -cpu=8 \
  -cpuprofile=cpu.out -mutexprofile=mu.out ./bench/mtaudit/
go tool pprof -peek='Lookup' mu.out      # → 100% of Mutex.Unlock delay in PropertyKeyRegistry.Lookup
```

Key source references: `graph/lpg/property.go:231` (`PropertyKeyRegistry.Lookup` — R1),
`graph/lpg/lpg.go:117` (`LabelRegistry.Lookup` — R2), `graph/lpg/edge_instance_props.go:99` (R3),
`graph/lpg/edge_instance_labels.go:107` (R4), `graph/mapper.go:299` (clean RWMutex reference),
`graph/lpg/lpg.go:1404` (M1 gate, shipped), `cypher/exec/parallel.go` (#1682 unwired scan),
`docs/isolation-design.md` (M2 COW spec).
```
