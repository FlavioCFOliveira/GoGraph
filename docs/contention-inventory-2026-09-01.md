# Contention inventory — 2026-09-01

Ranked, measured inventory of every contention site found across the GoGraph
module surface. Produced by rmp task #2679 in sprint 353, "GoGraph Optimization
Laboratory".

## How this was measured

| | |
|---|---|
| Instrument | `bench/contention` (rmp #2678), the repository's first committed use of Go's mutex and block profilers |
| HEAD | `3733f514`, branch `feature/353-gograph-optimization-laboratory` |
| Host | Apple M4, **10 cores**, 32 GiB, `go1.27.0 darwin/arm64` |
| Host load | loadavg **1.40** before the sweep, **5.65** after; no competing workload |
| Sweep | 19 workloads × 5 levels × 2 windows = 190 fresh child processes, 393.8 s wall, exit 0, **0 errors**, 95 rows |
| Ranking | `go tool pprof -top -cum -lines` — cumulative, line-granular |

Every measurement runs **two windows in two separate processes**: an unprofiled
window supplying throughput and latency, and a profiled window supplying lock
attribution. Both the profiles and the heap are cumulative per process with no
reset API, so sharing a process lets the first window contaminate the second —
when they shared one, the *profiled* window measured faster than the unprofiled
one in five runs of six.

### Reading the numbers honestly

- **The host has 10 cores.** Only the 1 → 8 region is a scaling signal. At 64 and
  above the question is not "does it go faster" (it cannot) but "does it degrade
  gracefully, and where does it block". Oversubscription is **not** reported here
  as a contention defect.
- **Anti-scaling at 8 goroutines is different, and it is damning.** With spare
  cores available, a curve that falls below 1.000× means the lock costs more than
  the work it protects.
- Harness frames (`chanrecv1`, `WaitGroup.Wait`) are excluded from attribution.
- `scaling_vs_1` is throughput at level *N* divided by throughput at level 1,
  both from the unprofiled window.

## Scaling table

`scaling_vs_1`, higher is better. **Bold** marks anti-scaling.

| workload | 1 | 8 | 64 | 256 | 1024 |
|---|---|---|---|---|---|
| `search-sssp-shared` | 1.000 | 5.376 | 6.758 | 7.001 | 6.930 |
| `search-bfs-csr` | 1.000 | 5.407 | 6.074 | 6.509 | 6.742 |
| `cypher-write-wal` | 1.000 | 3.990 | 31.793 | 97.792 | 279.779 |
| `store-checkpoint-write` | 1.000 | 3.893 | 22.938 | 50.018 | 108.450 |
| `cypher-read-scan-large` | 1.000 | 3.206 | 3.541 | 3.554 | 3.434 |
| `lpg-neighbours-read` | 1.000 | 3.039 | 3.208 | 3.368 | 3.225 |
| `cypher-read-project` | 1.000 | 3.017 | 3.690 | 3.600 | 3.601 |
| `mvcc-explicit-tx` | 1.000 | 2.118 | 2.157 | 1.596 | 1.157 |
| `mvcc-session-write` | 1.000 | 2.084 | 2.130 | 1.616 | 1.161 |
| `centrality-pagerank` | 1.000 | 2.068 | 3.344 | 3.626 | 3.588 |
| `cypher-write-mem` | 1.000 | 1.942 | 1.973 | 1.500 | 1.128 |
| `bolt-connect-churn` | 1.000 | 1.605 | 2.043 | 2.498 | 2.841 |
| `cypher-mixed-rw` | 1.000 | 1.602 | **0.691** | **0.223** | **0.096** |
| `bolt-wire-read` | 1.000 | 1.492 | 1.633 | 1.716 | 1.756 |
| `cypher-read-label-small` | 1.000 | 1.467 | 1.472 | 1.487 | 1.477 |
| `index-hash-rw` | 1.000 | 1.329 | 1.251 | 1.101 | **0.916** |
| `index-count-spread` | 1.000 | **0.917** | **0.665** | **0.609** | **0.538** |
| `index-btree-rw` | 1.000 | **0.445** | **0.359** | **0.199** | **0.153** |
| `index-count-hot` | 1.000 | **0.391** | **0.393** | **0.274** | **0.147** |

`cypher-write-wal` and `store-checkpoint-write` rise far above the core count.
That is **not** superlinear compute: it is **group commit amortising fsync**. At
level 1 every commit pays its own fsync; at 1024 the WAL coalesces many commits
into one. It is the durability layer working as designed, not a defect, and it
should not be quoted as a scaling result for the engine.

## Ranked contention sites

Ranked by severity: anti-scaling first, then absolute delay. "Share" is the
site's percentage of all mutex delay in that workload.

> **CORRECTION, same day.** Site 1 was mis-attributed when first written; the
> correction is recorded here rather than by silently rewriting the section. It was
> named on the strength of a **cumulative** ranking, but `lpg.go:1342` is
> `return 0, fn(WriteTx{w: w})` — the statement-body call — so a `-cum` share there
> says only "the delay is somewhere inside the write", which is close to tautological.
> `pprof -peek` shows the gate costs **microseconds** (`beginWrite` 6.83 µs,
> `finishWriteSharedInstant` 14.33 µs), and `graph/mvcc.Gate` is already a striped
> weak/strong gate with 64 cache-line-padded slots.
>
> **The real terminal site is `graph/lpg/lpg.go:2893` — the `sh.mu.Lock()` in
> `setNodeLabelInfo` — holding 14.48 s, 75.5% of all mutex delay in the write
> workload.** Chain: `CreateNode.Next` → `SetNodeLabel` (92.39%) →
> `setNodeLabelInfo` → `sync.RWMutex.Unlock` (94.53%). The node-label map is
> already sharded, so the lever is the **width of the critical section** — it spans
> the label-bag mutation, the MVCC write-write conflict test, delta stamping and
> `index/label.Index.Add` — not the shard count.

### 1 — The node-label write path · CRITICAL

| | |
|---|---|
| Site | `cypher/api.go:18333` `execUnderBarrier` → `graph/lpg/lpg.go:1342` `applyVersionedInstant` |
| Entry | `cypher/api.go:18023` `Engine.RunInTx` |
| Share | **80.68%** @ `cypher-write-mem@8`, **95.99%** @64, **99.70%** @1024 |
| Worsens with concurrency | **Yes, superlinearly** |

Total mutex delay for `cypher-write-mem` across the ladder:

| level | 1 | 8 | 64 | 256 | 1024 |
|---|---|---|---|---|---|
| delay | 4.53 ms | 662.53 ms | 19.17 s | 91.07 s | **402.96 s** |

**~89,000× the delay for 128× the goroutines.** That is the signature of a single
global barrier, not of honest oversubscription.

Its worst consequence is a **latency cliff** in the mixed read/write workload:
`cypher-mixed-rw` reaches 1.602× at 8 and then collapses to **0.096×** at 1024,
with 3314.69 s of mutex delay, 81.28% of it at this barrier. Writers under it
also block readers, which is why the mixed workload falls while pure reads do
not. Compliance Mandate 3 forbids precisely this: "no latency cliff", and
"readers do not block writers and vice versa where avoidable".

The same barrier dominates every write-shaped workload: `mvcc-explicit-tx`
(62.74% @8 → 94.31% @1024), `mvcc-session-write` via `cypher/session.go:109`
(73.30% → 99.58%), `store-checkpoint-write` (65.53% → 100%).

Tracked as **rmp #2681**. Removing it is an architecture change and requires an
agreed design before adoption.

### 2 — Count store, single-shard write path · HIGH

| | |
|---|---|
| Site | `graph/index/count/count.go:261` `Store.Apply` → `:318` `Store.add` |
| Share | **98.15%** @8 (19.43 s total), **100%** @1024 (**3.54 hours** total) |
| Scaling | 0.391× @8 — **anti-scaling** |

**The sweep contains its own control for the fix.** The identical store spread
across 64 shards (`index-count-spread`, 4096 relationship types) reaches 0.917×
where the single-shard arm reaches 0.391× — **2.35× from sharding alone**. Note
that `spread` still anti-scales, so 64 shards narrows the problem without
solving it.

Tracked as **rmp #2682**.

### 3 — B-tree index, one global RWMutex · HIGH

| | |
|---|---|
| Site | `graph/index/btree/index.go:213` `Index.Insert` (deferred unlock at `:211`) |
| Share | **97.71%** @8 (16.61 s total), **100%** @1024 (**2.06 hours** total) |
| Scaling | 0.445× @8 → 0.153× @1024 — **anti-scaling** |

Mandate 3 states that a hot path serialising every caller on a single global
lock is "a defect against this mandate, not merely a missed optimisation".

**Follow-up measurement corrects the cause.** Running the same workload with the
write fraction set to **zero** still collapses to **0.348× at 8** and then goes
flat (0.354 @64, 0.357 @1024). So the 1.000 → 0.35 cliff is the **reader's own
`RLock`/`RUnlock`** — two atomic read-modify-writes on one cache line, saturating
at ~9.3M pairs/s, a hardware coherence ceiling rather than queueing. Only the
further decay 0.368 → 0.162 is writer exclusion. Shortening the writer's critical
section alone therefore cannot fix this: **the read path must take no lock at
all.** A control arm with lock-free traversal but a global write mutex reached
7.9× on pure reads and only **1.6×** on the 90/10 mix — 10% of operations on a
global lock cost 4.6× of achieved throughput. Both halves must be fixed together.
Measured ceiling with zero read synchronisation on this host: **7.61×**, not 10×
(4 performance + 6 efficiency cores).

Tracked as **rmp #2683**.

### 4 — Hash index insert · MEDIUM

| | |
|---|---|
| Site | `graph/index/hash/index.go:185` `Index.Insert` |
| Share | 92.26% @8 (1.23 s), 98.22% @1024 (1126 s) |
| Scaling | 1.329× @8, decaying to 0.916× @1024 |

With 256 value-hashed shards this is **the only index in the sweep that scales at
all**, which is the positive evidence that sharding is the right structural
answer for the other two. It still decays past 256 goroutines.

### 5 — Cypher read path · MEDIUM

| | |
|---|---|
| Site | `cypher/api.go:2367` `Engine.Run`, via `runRead` `:2529` and `newResultWithLimit` `:6232`/`:6244` |
| Share | 68.62% @ `cypher-read-label-small@8`, 99.90% @1024 |
| Scaling | 1.467× @8 and flat thereafter |

A **read-only** query taking enough lock time to cap scaling at ~1.47× on ten
cores. `newResultWithLimit` accounts for 31.25% + 21.24% at level 8, pointing at
result construction rather than at graph access. Not yet tracked — needs its own
task.

### 6 — Bolt accept loop · MEDIUM

| | |
|---|---|
| Site | `bolt/server/serve.go:802` `Server.Serve.func3` |
| Share | 17.87% @ `bolt-connect-churn@8` → 79.67% @1024; **99.53%** @ `bolt-wire-read@1024` (194.70 s) |
| Scaling | `bolt-wire-read` 1.492× @8, flat to 1.756× |

Both Bolt workloads still improve with concurrency, so this is a ceiling rather
than a collapse. Not yet tracked.

### 7 — MVCC vacuum · LOW, workload-dependent

| | |
|---|---|
| Site | `graph/lpg/mvcc_vacuum.go:662` `vacuumLoop` → `:775` `vacuumPass` → `:830`/`:846` `sweepUnit` |
| Share | **2.42%** @ `cypher-write-mem@64`; **38.28%** @ `cypher-mixed-rw@8`; **17.47%** (579.19 s) @ `cypher-mixed-rw@1024` |

Secondary to the barrier on pure writes, but material in mixed workloads.

### 8 — PageRank worker · LOW

`search/centrality/pagerank.go:746`, 82.19% of a **864.90 ms** total at 1024. A
large share of a small number; the workload scales 3.588×. Recorded for
completeness, not actionable.

## Surface coverage

| Surface | Reached by | Verdict |
|---|---|---|
| `cypher` (read) | `cypher-read-label-small`, `-project`, `-scan-large` | site 5 |
| `cypher` (write) | `cypher-write-mem`, `-wal` | site 1 |
| `cypher/exec` | `cypher-read-scan-large` (parallel scan) | clean |
| `cypher` sessions | `mvcc-session-write`, `mvcc-explicit-tx` | site 1 |
| `graph/lpg` | `lpg-neighbours-read`, all cypher workloads | site 1, site 7 |
| `graph/adjlist` | `lpg-neighbours-read` | clean |
| `graph/mvcc` | `mvcc-session-write`, `mvcc-explicit-tx`, `cypher-mixed-rw` | site 1, site 7 |
| `graph/index/count` | `index-count-hot`, `index-count-spread` | **site 2** |
| `graph/index/btree` | `index-btree-rw` | **site 3** |
| `graph/index/hash` | `index-hash-rw` | site 4 |
| `search` | `search-bfs-csr`, `search-sssp-shared` | **cleanest surface** |
| `search/centrality` | `centrality-pagerank` | site 8 |
| `store/txn`, `store/wal` | `cypher-write-wal` | group commit, healthy |
| `store/checkpoint` | `store-checkpoint-write` | site 1 (inherited) |
| `bolt/server`, `bolt/packstream` | `bolt-wire-read`, `bolt-connect-churn` | site 6 |

**Not reached.** The DST simulator's own concurrent drivers, `sim.RunConcurrent`
and `sim.RunMVCCSessions`, were not wired into the sweep. The surfaces they
exercise — the engine under multi-session MVCC, and the Bolt wire — are covered
by the workloads above, but the DST's fault-injection and crash paths are
**not** represented in this inventory. `graph/generation`, `graph/index/manager`
and `internal/metrics` were likewise not driven directly.

**`search` is the module's best-behaved surface** and deserves saying so: 5.4×
at 8 goroutines, 6.7–6.9× at 1024, and at `search-bfs-csr@1024` **no module
frame appears in the mutex profile at all** — 6.49 ms of total delay, none of it
GoGraph's. Whatever was done there is the pattern the rest of the module should
follow.

## Leads refuted

Three inherited premises were re-validated at HEAD and **all three failed**.

1. **The `bench/mvccwrite` write-scaling collapse is dead.** That harness
   documents throughput falling to 0.828× at 32 writers and blames
   `cypher.Engine.writeMu` and `lpg.Graph.visMu`. At HEAD `cypher-write-mem`
   scales **1.942× at 8** and stays above 1.0 through 1024. The baseline predates
   the MVCC epic (head `c97118fe`, go1.26.5) and must not be quoted again.
2. **`mvcc_vacuum.go:662` is not the top write-path site.** An early reading put
   it at 30.64%. Across the full sweep it is **2.42%** at `cypher-write-mem@64`
   against the barrier's 95.99%. It is real but secondary — see site 7.
3. **`lpg.NodeLabels` heap-allocator contention is negligible.** An early reading
   showed "100% of mutex delay" via `mheap.alloc` — but that was 100% of a 2.1 ms
   total. Across the sweep it is **0.63 ms of 58.31 ms (1.08%)** at level 8, in a
   workload that scales 3.039×. The allocation is real; the contention is not
   material.

## Artefacts

Profiles (`cpu`, `mutex`, `block`, `goroutine`), `metrics.json` per window, and
`summary.tsv` are written under the sweep run directory reported by
`TestSweep`. Reproduce with:

```
GOGRAPH_CONTENTION_SWEEP_DIR=<abs dir> \
  go test -v -count=1 -timeout=180m -run TestSweep ./bench/contention/
```

`-v` is required: `go test` discards everything a passing package writes.

---

# Campaign results — clean re-measurement at `29860f7a`

Single sweep, isolated git worktree, no other work on the host. loadavg 2.88
before, 7.67 after (the sweep's own load). `SWEEP_EXIT=0`, zero failures, 71.2 s.

**Validity control.** `index-hash-rw` is untouched by this sprint and reads
**1.01 / 1.04 / 0.86 / 0.98 / 1.00** across the ladder — four of five levels
within 4%. An earlier attempt at this sweep read the same control at **0.40×**,
which is how it was caught as contaminated (another agent was compiling
concurrently) and discarded rather than published.

## Absolute throughput, pre-campaign → now

| workload | level | before (ops/s) | after (ops/s) | factor |
|---|---:|---:|---:|---:|
| `cypher-mixed-rw` | 1024 | 8,886 | 386,126 | **43.45×** |
| `cypher-mixed-rw` | 256 | 20,664 | 430,020 | **20.81×** |
| `cypher-mixed-rw` | 64 | 64,036 | 528,640 | **8.26×** |
| `index-btree-rw` | 1024 | 3,711,196 | 123,409,681 | **33.25×** |
| `index-btree-rw` | 256 | 4,836,808 | 126,082,095 | **26.07×** |
| `index-btree-rw` | 8 | 10,803,838 | 130,843,253 | **12.11×** |
| `index-count-spread` | 1024 | 54,454,380 | 296,297,978 | **5.44×** |
| `index-count-hot` | 1024 | 24,077,229 | 79,878,766 | **3.32×** |
| `cypher-write-mem` | 1024 | 418,595 | 490,518 | 1.17× |
| `mvcc-session-write` | 1024 | 423,863 | 468,346 | 1.10× |

## The anti-scaling is gone

`scaling_vs_1`, before → after:

| workload | 8 | 64 | 256 | 1024 |
|---|---|---|---|---|
| `index-btree-rw` | 0.445 → **5.610** | 0.359 → **5.668** | 0.199 → **5.406** | 0.153 → **5.291** |
| `index-count-spread` | 0.917 → **2.423** | 0.665 → **2.587** | 0.609 → **2.685** | 0.538 → **2.508** |
| `cypher-mixed-rw` | 1.602 → 1.203 | 0.691 → **1.142** | 0.223 → 0.929 | 0.096 → **0.834** |

`cypher-mixed-rw`'s ratios understate the result and must be read with the
absolutes beside them: its level-1 throughput itself rose **4.99×** (92,690 →
462,953), because a single reader now skips the suspect walk entirely. Every
rung improved; the ratio fell only because the denominator improved most.

`index-count-hot` holds a flat 0.325 across the whole ladder where it previously
decayed 0.391 → 0.147. A ratio above 1.000 is unreachable there by
construction — every writer increments one counter on one cache line, so the
bound is cache coherence, not locking.

## What was NOT fixed

* **`index-count-hot` cannot scale**, only stop collapsing (see above).
* **`cypher-mixed-rw` at 1024 is 0.834**, not above 1.000. The ceiling arm — the
  whole suspect machinery deleted — measured 0.760 at that rung, so the residual
  is oversubscription on 10 cores, not contention.
* **`graph/index/label`** still holds a global `RWMutex`. Its ceiling probe buys
  only ~4%, at or below the instrument's own repeatability, so it was measured
  and declined rather than shipped (rmp #2685).
* **A pre-existing ACID hole** was found and independently reproduced: a deleted
  node is briefly visible to present-time readers (rmp #2687). It predates this
  campaign and is unaffected by it.
